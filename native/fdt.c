#include "vmm.h"

#include <stdio.h>
#include <string.h>

struct fdt {
    uint8_t structure[32768], strings[4096];
    size_t pos, names;
    bool failed;
};

static uint32_t be32(uint32_t n) { return __builtin_bswap32(n); }

static void emit(struct fdt *f, const void *data, size_t len)
{
    size_t aligned = (len + 3) & ~(size_t)3;
    if (aligned > sizeof(f->structure) - f->pos) {
        f->failed = true;
        return;
    }
    memcpy(f->structure + f->pos, data, len);
    memset(f->structure + f->pos + len, 0, aligned - len);
    f->pos += aligned;
}

static void token(struct fdt *f, uint32_t n)
{
    n = be32(n);
    emit(f, &n, 4);
}

static void begin(struct fdt *f, const char *s)
{
    token(f, 1);
    emit(f, s, strlen(s) + 1);
}

static void end(struct fdt *f) { token(f, 2); }

static void prop(struct fdt *f, const char *name, const void *data, size_t len)
{
    size_t off = 0;
    while (off < f->names && strcmp((char *)f->strings + off, name))
        off += strlen((char *)f->strings + off) + 1;
    if (off == f->names) {
        size_t n = strlen(name) + 1;
        if (n > sizeof(f->strings) - f->names) {
            f->failed = true;
            return;
        }
        memcpy(f->strings + f->names, name, n);
        f->names += n;
    }
    token(f, 3);
    token(f, (uint32_t)len);
    token(f, (uint32_t)off);
    emit(f, data, len);
}

static void string(struct fdt *f, const char *name, const char *s)
{
    prop(f, name, s, strlen(s) + 1);
}

static void cells(struct fdt *f, const char *name, const uint32_t *v, size_t n)
{
    uint32_t data[32];
    if (n > 32) {
        f->failed = true;
        return;
    }
    for (size_t i = 0; i < n; ++i)
        data[i] = be32(v[i]);
    prop(f, name, data, n * 4);
}

#define CELLS(f, name, ...) do { \
    uint32_t values[] = {__VA_ARGS__}; \
    cells((f), (name), values, sizeof(values) / sizeof(values[0])); \
} while (0)

static void reg(struct fdt *f, uint64_t base, uint64_t size)
{
    CELLS(f, "reg", (uint32_t)(base >> 32), (uint32_t)base,
          (uint32_t)(size >> 32), (uint32_t)size);
}

int fdt_build(struct vm *vm, void *out, size_t cap, size_t *size)
{
    struct fdt tree = {0}, *f = &tree;
    char name[80];
    begin(f, "");
    CELLS(f, "#address-cells", 2);
    CELLS(f, "#size-cells", 2);
    CELLS(f, "interrupt-parent", 1);
    string(f, "compatible", "sapphire,hypervisor-arm64");
    string(f, "model", "Sapphire ARM64");
    begin(f, "chosen");
    string(f, "bootargs", "console=ttyAMA0 earlycon=pl011,mmio32,0x09000000 "
                         "rdinit=/init panic=1");
    string(f, "stdout-path", "/pl011@9000000");
    if (vm->opt.initialize_data_disk)
        prop(f, "sapphire,initialize-data-disk", "", 0);
    CELLS(f, "linux,initrd-start", (uint32_t)(vm->boot.initrd_start >> 32),
          (uint32_t)vm->boot.initrd_start);
    CELLS(f, "linux,initrd-end", (uint32_t)(vm->boot.initrd_end >> 32),
          (uint32_t)vm->boot.initrd_end);
    end(f);
    begin(f, "memory@40000000");
    string(f, "device_type", "memory");
    reg(f, RAM_BASE, vm->mem.size);
    end(f);
    begin(f, "cpus");
    CELLS(f, "#address-cells", 1);
    CELLS(f, "#size-cells", 0);
    for (unsigned i = 0; i < vm->opt.cpus; ++i) {
        snprintf(name, sizeof(name), "cpu@%x", i);
        begin(f, name);
        string(f, "device_type", "cpu");
        string(f, "compatible", "arm,arm-v8");
        string(f, "enable-method", "psci");
        CELLS(f, "reg", i);
        end(f);
    }
    end(f);
    begin(f, "psci");
    static const char psci[] = "arm,psci-1.0\0arm,psci-0.2";
    prop(f, "compatible", psci, sizeof(psci));
    string(f, "method", "hvc");
    end(f);
    begin(f, "intc@8000000");
    string(f, "compatible", "arm,gic-v3");
    CELLS(f, "#interrupt-cells", 3);
    CELLS(f, "#address-cells", 2);
    CELLS(f, "#size-cells", 2);
    CELLS(f, "phandle", 1);
    prop(f, "interrupt-controller", "", 0);
    prop(f, "ranges", "", 0);
    CELLS(f, "#redistributor-regions", 1);
    CELLS(f, "reg", 0, (uint32_t)GIC_DIST, 0, (uint32_t)vm->gic_dist_size,
          0, (uint32_t)GIC_REDIST, 0, (uint32_t)vm->gic_redist_size);
    end(f);
    begin(f, "timer");
    string(f, "compatible", "arm,armv8-timer");
    CELLS(f, "interrupts", 1, 13, 4, 1, 14, 4, 1, 11, 4, 1, 10, 4);
    prop(f, "always-on", "", 0);
    end(f);
    begin(f, "apb-clock");
    string(f, "compatible", "fixed-clock");
    CELLS(f, "#clock-cells", 0);
    CELLS(f, "clock-frequency", 24000000);
    CELLS(f, "phandle", 2);
    end(f);
    begin(f, "pl011@9000000");
    static const char uart[] = "arm,pl011\0arm,primecell";
    prop(f, "compatible", uart, sizeof(uart));
    reg(f, UART_BASE, 0x1000);
    CELLS(f, "interrupts", 0, UART_IRQ - 32, 4);
    CELLS(f, "clocks", 2, 2);
    static const char clocks[] = "uartclk\0apb_pclk";
    prop(f, "clock-names", clocks, sizeof(clocks));
    end(f);
    begin(f, "rtc@9010000");
    static const char rtc[] = "arm,pl031\0arm,primecell";
    prop(f, "compatible", rtc, sizeof(rtc));
    reg(f, RTC_BASE, 0x1000);
    CELLS(f, "interrupts", 0, RTC_IRQ - 32, 4);
    CELLS(f, "clocks", 2);
    string(f, "clock-names", "apb_pclk");
    end(f);
    for (unsigned i = 0; i < DEVICE_COUNT; ++i) {
        snprintf(name, sizeof(name), "virtio_mmio@%x",
                 (unsigned)VIRTIO_BASE + i * VIRTIO_STRIDE);
        begin(f, name);
        string(f, "compatible", "virtio,mmio");
        reg(f, VIRTIO_BASE + i * VIRTIO_STRIDE, VIRTIO_STRIDE);
        CELLS(f, "interrupts", 0, VIRTIO_IRQ + i - 32, 4);
        prop(f, "dma-coherent", "", 0);
        end(f);
    }
    end(f);
    token(f, 9);
    size_t total = 56 + f->pos + f->names;
    if (f->failed || total > cap || vm->opt.cpus == 0 ||
        vm->opt.cpus > MAX_CPUS)
        return -1;
    uint32_t header[] = {be32(0xd00dfeed), be32((uint32_t)total), be32(56),
        be32((uint32_t)(56 + f->pos)), be32(40), be32(17), be32(16), 0,
        be32((uint32_t)f->names), be32((uint32_t)f->pos)};
    memcpy(out, header, sizeof(header));
    memset((uint8_t *)out + 40, 0, 16);
    memcpy((uint8_t *)out + 56, f->structure, f->pos);
    memcpy((uint8_t *)out + 56 + f->pos, f->strings, f->names);
    *size = total;
    return 0;
}
