#include "vmm.h"

#include <Hypervisor/Hypervisor.h>
#include <Hypervisor/hv_gic.h>
#include <Hypervisor/hv_gic_config.h>
#include <inttypes.h>
#include <os/object.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#define MIB (1024u * 1024u)
#define CHECK(x) do { if (!(x)) { \
    fprintf(stderr, "%s:%d failed: %s\n", __FILE__, __LINE__, #x); \
    exit(1); \
} } while (0)

void vm_log(const char *format, ...)
{
    va_list args;
    va_start(args, format);
    vfprintf(stderr, format, args);
    fputc('\n', stderr);
    va_end(args);
}

static uint64_t pattern(size_t offset, unsigned cycle)
{
    return UINT64_C(0x9e3779b97f4a7c15) * (offset + 1) + cycle;
}

static uint64_t snapshot(const struct memory *m, const char *stage)
{
    /* host_statistics64 is cached; let it refresh between diagnostic stages. */
    struct timespec pause = {.tv_sec = 1, .tv_nsec = 100000000};
    nanosleep(&pause, NULL);
    uint64_t footprint = memory_snapshot(m, stage);
    CHECK(footprint != UINT64_MAX);
    return footprint;
}

static void fill(uint8_t *ram, size_t size, size_t page, unsigned cycle)
{
    for (size_t offset = 0; offset < size; offset += page) {
        st64(ram + offset, pattern(offset, cycle));
        st64(ram + offset + 8, pattern(0, cycle));
        st64(ram + offset + page - 8, ~pattern(offset, cycle));
    }
}

static void verify(uint8_t *ram, size_t begin, size_t end, size_t page, unsigned cycle)
{
    for (size_t offset = begin; offset < end; offset += page) {
        CHECK(ld64(ram + offset) == pattern(offset, cycle));
        CHECK(ld64(ram + offset + page - 8) == ~pattern(offset, cycle));
    }
}

static void guest_fill(hv_vcpu_t cpu, uint64_t code, size_t size, size_t page,
                       uint64_t value, hv_vcpu_exit_t *cpu_exit)
{
    CHECK(hv_vcpu_set_reg(cpu, HV_REG_PC, code) == HV_SUCCESS);
    CHECK(hv_vcpu_set_reg(cpu, HV_REG_X0, RAM_BASE) == HV_SUCCESS);
    CHECK(hv_vcpu_set_reg(cpu, HV_REG_X1, size / page) == HV_SUCCESS);
    CHECK(hv_vcpu_set_reg(cpu, HV_REG_X2, page) == HV_SUCCESS);
    CHECK(hv_vcpu_set_reg(cpu, HV_REG_X3, pattern(0, (unsigned)value - 1)) == HV_SUCCESS);
    CHECK(hv_vcpu_set_reg(cpu, HV_REG_X5, value) == HV_SUCCESS);
    CHECK(hv_vcpu_set_reg(cpu, HV_REG_CPSR, 0x3c5) == HV_SUCCESS);
    CHECK(hv_vcpu_set_sys_reg(cpu, HV_SYS_REG_SCTLR_EL1, 0x30d00800) == HV_SUCCESS);
    CHECK(hv_vcpu_run(cpu) == HV_SUCCESS);
    CHECK(cpu_exit->reason == HV_EXIT_REASON_EXCEPTION);
    CHECK((cpu_exit->exception.syndrome >> 26) == 0x16);
    uint64_t failed = 1;
    CHECK(hv_vcpu_get_reg(cpu, HV_REG_X6, &failed) == HV_SUCCESS);
    CHECK(!failed);
}

int main(int argc, char **argv)
{
    size_t size = (argc > 1 ? strtoul(argv[1], NULL, 10) : 512) * MIB;
    unsigned cycles = argc > 2 ? (unsigned)strtoul(argv[2], NULL, 10) : 6;
    size_t page = (size_t)sysconf(_SC_PAGESIZE), chunk = 2u * MIB;
    CHECK(size >= 16u * MIB && size <= 4096ull * MIB && size % chunk == 0);
    CHECK(cycles && cycles <= 32);
    struct vm vm = {0};
    vm.reclaim = hv_memory_reclaim;
    snapshot(&vm.mem, "before-memory-create");
    CHECK(!memory_create(&vm.mem, size));
    snapshot(&vm.mem, "after-memory-create");
    CHECK(hv_vm_create(NULL) == HV_SUCCESS);
    CHECK(hv_vm_map(vm.mem.ram, RAM_BASE, size,
                   HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC) == HV_SUCCESS);
    snapshot(&vm.mem, "after-hv-map");
    hv_gic_config_t config = hv_gic_config_create();
    CHECK(config);
    CHECK(hv_gic_config_set_distributor_base(config, GIC_DIST) == HV_SUCCESS);
    CHECK(hv_gic_config_set_redistributor_base(config, GIC_REDIST) == HV_SUCCESS);
    CHECK(hv_gic_create(config) == HV_SUCCESS);
    os_release(config);
    uint64_t base = snapshot(&vm.mem, "after-gic-create");
    fill(vm.mem.ram, size, page, 0);
    /*
     * ldr x4,[x0,#8]; cmp x4,x3; b.ne fail; str x5,[x0];
     * add x0,x0,x2; subs x1,x1,#1; b.ne loop;
     * mov x6,#0; hvc #0; fail: mov x6,#1; hvc #1.
     */
    const uint32_t code[] = {0xf9400404, 0xeb03009f, 0x540000e1, 0xf9000005,
        0x8b020000, 0xf1000421, 0x54ffff41, 0xd2800006, 0xd4000002,
        0xd2800026, 0xd4000022};
    size_t code_offset = size - page + 64;
    memcpy(vm.mem.ram + code_offset, code, sizeof(code));
    __builtin___clear_cache((char *)vm.mem.ram + code_offset,
                            (char *)vm.mem.ram + code_offset + sizeof(code));
    hv_vcpu_t cpu;
    hv_vcpu_exit_t *cpu_exit;
    CHECK(hv_vcpu_create(&cpu, &cpu_exit, NULL) == HV_SUCCESS);
    uint64_t maximum_loaded = 0, maximum_released = 0;
    for (unsigned cycle = 0; cycle < cycles; ++cycle) {
        fill(vm.mem.ram, size - chunk, page, cycle);
        if (!cycle)
            snapshot(&vm.mem, "host-touched");
        guest_fill(cpu, RAM_BASE + code_offset, size - chunk, page, cycle + 1, cpu_exit);
        for (size_t offset = 0; offset < size - chunk; offset += page) {
            CHECK(ld64(vm.mem.ram + offset) == cycle + 1);
            CHECK(ld64(vm.mem.ram + offset + 8) == pattern(0, cycle));
            CHECK(ld64(vm.mem.ram + offset + page - 8) == ~pattern(offset, cycle));
        }
        verify(vm.mem.ram, size - chunk, size, page, 0);
        char label[64];
        snprintf(label, sizeof(label), "loaded-%u", cycle);
        uint64_t loaded = snapshot(&vm.mem, label);
        CHECK(loaded <= base + 2 * size + 32u * MIB);
        if (loaded > maximum_loaded)
            maximum_loaded = loaded;
        for (size_t offset = 0; offset < size - chunk; offset += chunk)
            CHECK(!memory_report_free(&vm, RAM_BASE + offset, chunk));
        verify(vm.mem.ram, size - chunk, size, page, 0);
        snprintf(label, sizeof(label), "released-%u", cycle);
        uint64_t released = snapshot(&vm.mem, label);
        CHECK(released <= base + chunk + 16u * MIB);
        if (released > maximum_released)
            maximum_released = released;
    }
    fill(vm.mem.ram, size - chunk, page, cycles);
    guest_fill(cpu, RAM_BASE + code_offset, size - chunk, page, cycles + 1, cpu_exit);
    snapshot(&vm.mem, "teardown-loaded");
    CHECK(hv_vcpu_destroy(cpu) == HV_SUCCESS);
    snapshot(&vm.mem, "after-vcpu-destroy");
    snapshot(&vm.mem, "before-hv-unmap");
    CHECK(hv_vm_unmap(RAM_BASE, size) == HV_SUCCESS);
    snapshot(&vm.mem, "after-hv-unmap");
    CHECK(hv_vm_destroy() == HV_SUCCESS);
    snapshot(&vm.mem, "after-hv-destroy");
    memory_destroy(&vm.mem);
    uint64_t final = snapshot(&vm.mem, "after-memory-destroy");
    CHECK(final <= base + 16u * MIB);
    printf("{\"ram_bytes\":%zu,\"cycles\":%u,\"baseline\":%" PRIu64
           ",\"maximum_loaded\":%" PRIu64 ",\"maximum_released\":%" PRIu64
           ",\"final\":%" PRIu64 ",\"coherence_and_live_canaries\":true,"
           "\"released_charge_bounded\":true}\n",
           size, cycles, base, maximum_loaded, maximum_released, final);
    return 0;
}
