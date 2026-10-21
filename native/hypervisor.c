#include "vmm.h"

#include <Hypervisor/Hypervisor.h>
#include <Hypervisor/hv_gic.h>
#include <Hypervisor/hv_gic_config.h>
#include <Hypervisor/hv_gic_parameters.h>
#include <errno.h>
#include <inttypes.h>
#include <os/object.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

struct cpu {
    struct vm *vm;
    pthread_t thread;
    hv_vcpu_t id;
    hv_vcpu_exit_t *exit;
    unsigned index;
    bool created, started;
    unsigned state; /* 0 off, 1 pending, 2 on */
    uint64_t entry, context, exits;
};

struct hypervisor {
    pthread_mutex_t lock;
    pthread_cond_t cond;
    struct cpu *cpu;
    unsigned ready, count;
    bool created, mapped;
};

static bool check(hv_return_t result, const char *name)
{
    if (result == HV_SUCCESS)
        return true;
    vm_log("%s: Hypervisor error 0x%x", name, (unsigned)result);
    return false;
}

void vm_request_stop(struct vm *vm, int reason)
{
    int expected = 0;
    atomic_compare_exchange_strong(&vm->stop, &expected, reason);
    struct hypervisor *h = vm->hv;
    if (h) {
        pthread_mutex_lock(&h->lock);
        if (h->cpu)
            for (unsigned i = 0; i < h->count; ++i)
                if (h->cpu[i].created)
                    (void)hv_vcpus_exit(&h->cpu[i].id, 1);
        pthread_cond_broadcast(&h->cond);
        pthread_mutex_unlock(&h->lock);
    }
    if (vm->wake)
        vm->wake(vm);
}

static void irq(struct vm *vm, unsigned intid, bool level)
{
    if (!check(hv_gic_set_spi(intid, level), "hv_gic_set_spi"))
        vm_request_stop(vm, 3);
}

int hv_memory_reclaim(struct vm *vm, uint64_t gpa, size_t size)
{
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    void *addr = guest_ptr(&vm->mem, gpa, size);
    if (!addr || !size || (gpa & (page - 1)) || (size & (page - 1)))
        return -1;
    /*
     * Linux holds reporting pages out of its allocator until used completion.
     * Remove stage-2 aliases before replacing anonymous backing. MADV_FREE alone
     * is lazy on Darwin; mapping-operation counts do not measure physical release.
     */
    if (!check(hv_vm_unmap(gpa, size), "unmap reported free pages"))
        return -1;
    int rc = madvise(addr, size, MADV_FREE);
    if (!rc && mmap(addr, size, PROT_READ | PROT_WRITE,
                    MAP_FIXED | MAP_PRIVATE | MAP_ANON, -1, 0) == MAP_FAILED)
        rc = -1;
    bool mapped = check(hv_vm_map(addr, gpa, size,
        HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC), "remap reported free pages");
    return rc || !mapped ? -1 : 0;
}

static bool reset_cpu(struct cpu *c)
{
    for (unsigned r = HV_REG_X0; r <= HV_REG_X30; ++r)
        if (!check(hv_vcpu_set_reg(c->id, (hv_reg_t)r, 0), "reset general register"))
            return false;
    return check(hv_vcpu_set_reg(c->id, HV_REG_PC, c->entry), "set PC") &&
        check(hv_vcpu_set_reg(c->id, HV_REG_X0, c->context), "set x0") &&
        check(hv_vcpu_set_reg(c->id, HV_REG_CPSR, 0x3c5), "set EL1h PSTATE") &&
        check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_SCTLR_EL1, 0x30d00800), "set SCTLR") &&
        check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_TCR_EL1, 0), "set TCR") &&
        check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_TTBR0_EL1, 0), "set TTBR0") &&
        check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_TTBR1_EL1, 0), "set TTBR1") &&
        check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_CPACR_EL1, 3u << 20), "set CPACR") &&
        check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_CNTV_CTL_EL0, 0), "reset CNTV") &&
        check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_CNTP_CTL_EL0, 0), "reset CNTP") &&
        check(hv_vcpu_set_vtimer_offset(c->id, 0), "set virtual counter offset");
}

static int64_t psci(struct cpu *c, uint64_t fn, uint64_t a1, uint64_t a2, uint64_t a3)
{
    struct vm *vm = c->vm;
    struct hypervisor *h = vm->hv;
    if (!(fn & UINT64_C(0x40000000))) {
        a1 = (uint32_t)a1;
        a2 = (uint32_t)a2;
        a3 = (uint32_t)a3;
    }
    switch ((uint32_t)fn) {
    case 0x84000000: return 0x00010000; /* PSCI 1.0 */
    case 0x84000001:
    case 0xc4000001: return -1; /* CPU_SUSPEND is not advertised. */
    case 0x84000002:
        pthread_mutex_lock(&h->lock);
        c->state = 0;
        pthread_mutex_unlock(&h->lock);
        return 0;
    case 0x84000003:
    case 0xc4000003: {
        if (a1 >= h->count)
            return -2;
        if ((a2 & 3) || !guest_ptr(&vm->mem, a2, 4))
            return -9;
        pthread_mutex_lock(&h->lock);
        struct cpu *target = &h->cpu[a1];
        int64_t result;
        if (target->state)
            result = target->state == 1 ? -5 : -4;
        else {
            target->entry = a2;
            target->context = a3;
            target->state = 1;
            pthread_cond_broadcast(&h->cond);
            result = 0;
        }
        pthread_mutex_unlock(&h->lock);
        return result;
    }
    case 0x84000004:
    case 0xc4000004: {
        if (a1 >= h->count || a2 != 0)
            return -2;
        pthread_mutex_lock(&h->lock);
        unsigned state = h->cpu[a1].state;
        pthread_mutex_unlock(&h->lock);
        return state == 0 ? 1 : state == 1 ? 2 : 0;
    }
    case 0x84000006: return 2; /* MIGRATE_INFO_TYPE: no trusted OS. */
    case 0x84000008: vm_request_stop(vm, 1); return 0;
    case 0x84000009: vm_request_stop(vm, 2); return 0;
    case 0x8400000a: /* PSCI_FEATURES */
        switch ((uint32_t)a1) {
        case 0x84000000: case 0x84000002: case 0x84000003: case 0xc4000003:
        case 0x84000004: case 0xc4000004: case 0x84000006: case 0x84000008:
        case 0x84000009: case 0x8400000a:
            return 0;
        default: return -1;
        }
    case 0x80000000: return 0x00010001; /* SMCCC_VERSION */
    case 0x80000001: return -1; /* SMCCC_ARCH_FEATURES */
    default: return -1;
    }
}

static bool advance(struct cpu *c, uint64_t pc)
{
    return check(hv_vcpu_set_reg(c->id, HV_REG_PC, pc + 4), "advance PC");
}

static bool exception(struct cpu *c)
{
    uint64_t esr = c->exit->exception.syndrome, pc = 0;
    unsigned ec = (unsigned)(esr >> 26) & 63;
    if (!check(hv_vcpu_get_reg(c->id, HV_REG_PC, &pc), "get PC"))
        return false;
    if (ec == 0x16) { /* HVC64; the architectural ELR points past HVC. */
        uint64_t args[4];
        for (unsigned i = 0; i < 4; ++i)
            if (!check(hv_vcpu_get_reg(c->id, (hv_reg_t)i, &args[i]), "get HVC argument"))
                return false;
        int64_t result = psci(c, args[0], args[1], args[2], args[3]);
        return check(hv_vcpu_set_reg(c->id, HV_REG_X0, (uint64_t)result), "set HVC result");
    }
    if (ec == 0x18) {
        unsigned op0 = (unsigned)(esr >> 20) & 3;
        unsigned op1 = (unsigned)(esr >> 14) & 7;
        unsigned op2 = (unsigned)(esr >> 17) & 7;
        unsigned crn = (unsigned)(esr >> 10) & 15;
        unsigned crm = (unsigned)(esr >> 1) & 15;
        unsigned rt = (unsigned)(esr >> 5) & 31;
        unsigned sysreg = (op0 << 14) | (op1 << 11) | (crn << 7) | (crm << 3) | op2;
        /* External debug OS locks are absent. Linux clears them during bringup. */
        if (sysreg == 0x8084 || sysreg == 0x808c || sysreg == 0x809c) {
            if ((esr & 1) && rt != 31 &&
                !check(hv_vcpu_set_reg(c->id, (hv_reg_t)rt, 0), "read absent debug OS lock"))
                return false;
            return advance(c, pc);
        }
    }
    if (ec == 0x24 && (esr & (1u << 24)) && !(esr & (1u << 10))) {
        unsigned size = 1u << ((esr >> 22) & 3);
        unsigned reg = (unsigned)(esr >> 16) & 31;
        bool write = esr & (1u << 6);
        uint64_t value = 0, addr = c->exit->exception.physical_address;
        if (write && reg != 31 &&
            !check(hv_vcpu_get_reg(c->id, (hv_reg_t)reg, &value), "get MMIO operand"))
            return false;
        if (!mmio_access(c->vm, addr, size, write, &value)) {
            vm_log("vCPU %u unhandled MMIO %s at 0x%" PRIx64 " PC=0x%" PRIx64,
                   c->index, write ? "write" : "read", addr, pc);
            return false;
        }
        if (!write && reg != 31) {
            if (size < 8)
                value &= (UINT64_C(1) << (size * 8)) - 1;
            if (esr & (1u << 21)) {
                unsigned shift = 64 - size * 8;
                value = (uint64_t)((int64_t)(value << shift) >> shift);
            }
            if (!(esr & (1u << 15)))
                value = (uint32_t)value;
            if (!check(hv_vcpu_set_reg(c->id, (hv_reg_t)reg, value), "set MMIO result"))
                return false;
        }
        return advance(c, pc);
    }
    vm_log("vCPU %u unhandled exception EC=0x%x ESR=0x%" PRIx64
           " PC=0x%" PRIx64 " FAR=0x%" PRIx64 " IPA=0x%" PRIx64,
           c->index, ec, esr, pc, c->exit->exception.virtual_address,
           c->exit->exception.physical_address);
    return false;
}

static void *cpu_thread(void *arg)
{
    struct cpu *c = arg;
    struct vm *vm = c->vm;
    struct hypervisor *h = vm->hv;
    char name[32];
    snprintf(name, sizeof(name), "sapphire-vcpu-%u", c->index);
    pthread_setname_np(name);
    bool created = check(hv_vcpu_create(&c->id, &c->exit, NULL), "hv_vcpu_create");
    bool ok = created;
    if (ok) {
        ok = check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_MPIDR_EL1,
                                      UINT64_C(0x80000000) | c->index), "set MPIDR");
        uint64_t dfr;
        if (ok && hv_vcpu_get_sys_reg(c->id, HV_SYS_REG_ID_AA64DFR0_EL1, &dfr) == HV_SUCCESS)
            ok = check(hv_vcpu_set_sys_reg(c->id, HV_SYS_REG_ID_AA64DFR0_EL1,
                        dfr & ~UINT64_C(0xf00)), "disable unimplemented PMU");
    }
    pthread_mutex_lock(&h->lock);
    c->created = created;
    h->ready++;
    pthread_cond_broadcast(&h->cond);
    pthread_mutex_unlock(&h->lock);
    if (!ok)
        vm_request_stop(vm, 3);
    pthread_mutex_lock(&h->lock);
    while (h->ready < h->count && !atomic_load(&vm->stop))
        pthread_cond_wait(&h->cond, &h->lock);
    pthread_mutex_unlock(&h->lock);
    while (ok && !atomic_load(&vm->stop)) {
        pthread_mutex_lock(&h->lock);
        while (!c->state && !atomic_load(&vm->stop))
            pthread_cond_wait(&h->cond, &h->lock);
        bool pending = c->state == 1;
        if (pending)
            c->state = 2;
        pthread_mutex_unlock(&h->lock);
        if (atomic_load(&vm->stop))
            break;
        if (pending && !reset_cpu(c))
            break;
        if (!check(hv_vcpu_run(c->id), "hv_vcpu_run"))
            break;
        c->exits++;
        switch (c->exit->reason) {
        case HV_EXIT_REASON_CANCELED: break;
        case HV_EXIT_REASON_EXCEPTION: ok = exception(c); break;
        default:
            vm_log("vCPU %u unexpected exit %u with native GIC",
                   c->index, (unsigned)c->exit->reason);
            ok = false;
            break;
        }
    }
    if (!atomic_load(&vm->stop))
        vm_request_stop(vm, 3);
    if (c->created) {
        uint64_t time = 0;
        (void)hv_vcpu_get_exec_time(c->id, &time);
        vm_log("vCPU %u exits=%" PRIu64 " execution_ticks=%" PRIu64,
               c->index, c->exits, time);
        /* Serialize teardown against another thread's hv_vcpus_exit. */
        pthread_mutex_lock(&h->lock);
        (void)hv_vcpu_destroy(c->id);
        c->created = false;
        pthread_mutex_unlock(&h->lock);
    }
    return NULL;
}

int hv_start(struct vm *vm)
{
    uint32_t maximum = 0;
    if (!check(hv_vm_get_max_vcpu_count(&maximum), "get maximum vCPU count") ||
        vm->opt.cpus > maximum) {
        vm_log("requested %u vCPUs; host maximum is %u", vm->opt.cpus, maximum);
        return -1;
    }
    struct hypervisor *h = calloc(1, sizeof(*h));
    if (!h)
        return -1;
    vm->hv = h;
    pthread_mutex_init(&h->lock, NULL);
    pthread_cond_init(&h->cond, NULL);
    h->count = vm->opt.cpus;
    h->cpu = calloc(h->count, sizeof(*h->cpu));
    if (!h->cpu)
        return -1;
    uint32_t ipa_bits = 0, maximum_bits = 0;
    uint64_t last = RAM_BASE + vm->mem.size - 1;
    if (!check(hv_vm_config_get_default_ipa_size(&ipa_bits), "get default IPA size") ||
        !check(hv_vm_config_get_max_ipa_size(&maximum_bits), "get maximum IPA size"))
        return -1;
    unsigned needed = 64u - (unsigned)__builtin_clzll(last);
    if (needed > maximum_bits) {
        vm_log("guest RAM exceeds host IPA address space");
        return -1;
    }
    hv_vm_config_t vm_config = hv_vm_config_create();
    if (!vm_config)
        return -1;
    bool config_ok = needed <= ipa_bits ||
        check(hv_vm_config_set_ipa_size(vm_config, needed), "configure IPA address space");
    bool vm_ok = config_ok && check(hv_vm_create(vm_config), "hv_vm_create");
    os_release(vm_config);
    if (!vm_ok)
        return -1;
    h->created = true;
    if (!check(hv_vm_map(vm->mem.ram, RAM_BASE, vm->mem.size,
                        HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC), "hv_vm_map"))
        return -1;
    h->mapped = true;
    if (vm->opt.memory_diagnostics)
        memory_snapshot(&vm->mem, "after-hv-map");
    hv_gic_config_t config = hv_gic_config_create();
    if (!config)
        return -1;
    bool ok = check(hv_gic_config_set_distributor_base(config, GIC_DIST), "GIC distributor base") &&
        check(hv_gic_config_set_redistributor_base(config, GIC_REDIST), "GIC redistributor base") &&
        check(hv_gic_create(config), "hv_gic_create");
    os_release(config);
    if (vm->opt.memory_diagnostics)
        memory_snapshot(&vm->mem, "after-gic-create");
    if (!ok || !check(hv_gic_get_distributor_size(&vm->gic_dist_size), "GIC distributor size") ||
        !check(hv_gic_get_redistributor_region_size(&vm->gic_redist_size), "GIC redistributor size") ||
        GIC_REDIST + vm->gic_redist_size > RAM_BASE)
        return -1;
    vm->set_irq = irq;
    vm->reclaim = hv_memory_reclaim;
    if (boot_load(vm))
        return -1;
    for (unsigned i = 0; i < h->count; ++i) {
        struct cpu *c = &h->cpu[i];
        c->vm = vm;
        c->index = i;
        c->entry = vm->boot.entry;
        c->context = i == 0 ? vm->boot.dtb : 0;
        c->state = i == 0 ? 1 : 0;
        int error = pthread_create(&c->thread, NULL, cpu_thread, c);
        if (error) {
            vm_log("pthread_create: %s", strerror(error));
            vm_request_stop(vm, 3);
            return -1;
        }
        c->started = true;
    }
    return 0;
}

void hv_stop(struct vm *vm)
{
    struct hypervisor *h = vm->hv;
    if (!h)
        return;
    vm_request_stop(vm, 1);
    if (h->cpu) {
        for (unsigned i = 0; i < h->count; ++i)
            if (h->cpu[i].started)
                pthread_join(h->cpu[i].thread, NULL);
        free(h->cpu);
    }
    vm->set_irq = NULL;
    vm->reclaim = NULL;
    if (h->mapped)
        (void)hv_vm_unmap(RAM_BASE, vm->mem.size);
    if (vm->opt.memory_diagnostics)
        memory_snapshot(&vm->mem, "after-hv-unmap");
    if (h->created)
        (void)hv_vm_destroy();
    if (vm->opt.memory_diagnostics)
        memory_snapshot(&vm->mem, "after-hv-destroy");
    pthread_cond_destroy(&h->cond);
    pthread_mutex_destroy(&h->lock);
    free(h);
    vm->hv = NULL;
}

int hv_probe(unsigned cpus)
{
    uint32_t max = 0;
    if (!check(hv_vm_get_max_vcpu_count(&max), "get maximum vCPU count") ||
        !check(hv_vm_create(NULL), "hv_vm_create"))
        return 1;
    hv_gic_config_t config = hv_gic_config_create();
    bool ok = config &&
        check(hv_gic_config_set_distributor_base(config, GIC_DIST), "GIC distributor base") &&
        check(hv_gic_config_set_redistributor_base(config, GIC_REDIST), "GIC redistributor base") &&
        check(hv_gic_create(config), "hv_gic_create");
    if (config)
        os_release(config);
    if (ok) {
        hv_vcpu_t id;
        hv_vcpu_exit_t *exit;
        ok = check(hv_vcpu_create(&id, &exit, NULL), "hv_vcpu_create");
        if (ok)
            ok = check(hv_vcpu_destroy(id), "hv_vcpu_destroy");
    }
    ok = check(hv_vm_destroy(), "hv_vm_destroy") && ok;
    if (ok)
        printf("{\"hypervisor\":\"Hypervisor.framework\",\"gic\":3,"
               "\"max_cpus\":%u,\"requested_cpus\":%u,\"balloon_reporting\":true}\n",
               max, cpus);
    return ok && cpus <= max ? 0 : 1;
}
