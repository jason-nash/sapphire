#include "vmm.h"

#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

static unsigned assertions, interrupts, irq_calls, wakeups;
static bool verbose;
#define CHECK(x) do { \
    assertions++; \
    if (!(x)) { \
        fprintf(stderr, "%s:%d failed: %s\n", __FILE__, __LINE__, #x); \
        abort(); \
    } \
} while (0)

void vm_log(const char *fmt, ...)
{
    if (verbose) {
        va_list ap;
        va_start(ap, fmt);
        vfprintf(stderr, fmt, ap);
        fputc('\n', stderr);
        va_end(ap);
    }
}

void vm_request_stop(struct vm *vm, int reason) { atomic_store(&vm->stop, reason); }

static void test_irq(struct vm *vm, unsigned id, bool level)
{
    (void)vm;
    CHECK(id >= 32);
    irq_calls++;
    if (level)
        interrupts++;
}

static void test_wake(struct vm *vm)
{
    (void)vm;
    wakeups++;
}

static void init(struct vm *vm)
{
    memset(vm, 0, sizeof(*vm));
    vm->console_fd = vm->kq = vm->listener = -1;
    CHECK(!pthread_mutex_init(&vm->io_lock, NULL));
    CHECK(!memory_create(&vm->mem, 16u * 1024u * 1024u));
    for (unsigned i = 0; i < DEVICE_COUNT; ++i) {
        struct device *d = &vm->dev[i];
        d->disk_fd = -1;
        d->index = i;
        d->irq = VIRTIO_IRQ + i;
        d->status = 15;
        d->features = d->driver_features = UINT64_C(1) << 32;
        d->queue_count = QUEUES_MAX;
    }
    vm->set_irq = test_irq;
    vsock_init(vm);
}

static void destroy(struct vm *vm)
{
    devices_destroy(vm);
    if (vm->kq >= 0)
        close(vm->kq);
    memory_destroy(&vm->mem);
    pthread_mutex_destroy(&vm->io_lock);
}

static uint64_t base(unsigned device, unsigned queue)
{
    return RAM_BASE + 0x10000 + device * 0x20000 + queue * 0x8000;
}

static struct virtq *setup_queue(struct vm *vm, unsigned device, unsigned qi)
{
    struct virtq *q = &vm->dev[device].queue[qi];
    uint64_t start = base(device, qi);
    *q = (struct virtq){.num = 8, .desc = start, .avail = start + 0x1000,
        .used = start + 0x2000, .ready = true};
    memset(guest_ptr(&vm->mem, start, 0x8000), 0, 0x8000);
    vm->dev[device].status = 15;
    return q;
}

static void desc(struct vm *vm, struct virtq *q, unsigned idx, uint64_t addr,
                  uint32_t len, uint16_t flags, uint16_t next)
{
    uint8_t *p = guest_ptr(&vm->mem, q->desc + idx * 16, 16);
    CHECK(p != NULL);
    st64(p, addr);
    st32(p + 8, len);
    st16(p + 12, flags);
    st16(p + 14, next);
}

static void post(struct vm *vm, struct virtq *q, uint16_t head)
{
    uint8_t *p = guest_ptr(&vm->mem, q->avail, 6 + q->num * 2);
    uint16_t idx = ld16(p + 2);
    st16(p + 4 + 2 * (idx % q->num), head);
    st16(p + 2, idx + 1);
}

static void test_bounds(void)
{
    struct vm vm;
    init(&vm);
    CHECK(guest_ptr(&vm.mem, RAM_BASE, vm.mem.size) == vm.mem.ram);
    CHECK(guest_ptr(&vm.mem, RAM_BASE - 1, 1) == NULL);
    CHECK(guest_ptr(&vm.mem, UINT64_MAX, 1) == NULL);
    CHECK(guest_ptr(&vm.mem, RAM_BASE + vm.mem.size - 1, 2) == NULL);
    CHECK(guest_ptr(&vm.mem, RAM_BASE + vm.mem.size, 0) != NULL);
    CHECK(guest_ptr(&vm.mem, RAM_BASE, SIZE_MAX) == NULL);
    struct device *d = &vm.dev[0];
    struct virtq *q = setup_queue(&vm, 0, 0);
    struct chain c;
    desc(&vm, q, 0, q->desc + 0x3000, 8, 1, 1);
    desc(&vm, q, 1, q->desc + 0x3100, 8, 2, 0);
    memcpy(guest_ptr(&vm.mem, q->desc + 0x3000, 8), "12345678", 8);
    post(&vm, q, 0);
    CHECK(queue_pop(&vm, d, q, &c) == 1);
    CHECK(c.count == 2 && c.readable == 8 && c.writable == 8);
    char buf[8];
    CHECK(chain_read(&c, 2, buf, 6) == 6 && !memcmp(buf, "345678", 6));
    CHECK(chain_write(&c, 0, "ABCDEFGH", 8) == 8);
    CHECK(!memcmp(guest_ptr(&vm.mem, q->desc + 0x3100, 8), "ABCDEFGH", 8));
    queue_complete(&vm, d, q, &c, 8);
    CHECK(ld16(guest_ptr(&vm.mem, q->used + 2, 2)) == 1);
    CHECK(ld32(guest_ptr(&vm.mem, q->used + 8, 4)) == 8);
    CHECK(queue_pop(&vm, d, q, &c) == 0);

    q = setup_queue(&vm, 0, 0);
    desc(&vm, q, 0, q->desc + 0x3000, 8, 1, 0);
    post(&vm, q, 0);
    CHECK(queue_pop(&vm, d, q, &c) == -1 && (d->status & 64));
    q = setup_queue(&vm, 0, 0);
    desc(&vm, q, 0, UINT64_MAX - 4, 16, 0, 0);
    post(&vm, q, 0);
    CHECK(queue_pop(&vm, d, q, &c) == -1);
    q = setup_queue(&vm, 0, 0);
    desc(&vm, q, 0, q->desc + 0x3000, 8, 4, 0);
    post(&vm, q, 0);
    CHECK(queue_pop(&vm, d, q, &c) == -1); /* Indirect not advertised. */
    q = setup_queue(&vm, 0, 0);
    desc(&vm, q, 0, q->desc + 0x3000, 8, 3, 1);
    desc(&vm, q, 1, q->desc + 0x3100, 8, 0, 0);
    post(&vm, q, 0);
    CHECK(queue_pop(&vm, d, q, &c) == -1); /* Writable before readable. */
    q = setup_queue(&vm, 0, 0);
    st16(guest_ptr(&vm.mem, q->avail + 2, 2), 9);
    CHECK(queue_pop(&vm, d, q, &c) == -1);
    q = setup_queue(&vm, 0, 0);
    q->num = 3;
    CHECK(!queue_valid(&vm, q));
    q = setup_queue(&vm, 0, 0);
    q->desc++;
    CHECK(!queue_valid(&vm, q));

    /* Indices wrap at 16 bits without treating an old ring slot as new. */
    q = setup_queue(&vm, 0, 0);
    q->last_avail = q->next_used = UINT16_MAX;
    st16(guest_ptr(&vm.mem, q->avail + 2, 2), UINT16_MAX);
    desc(&vm, q, 0, q->desc + 0x3000, 1, 2, 0);
    post(&vm, q, 0);
    CHECK(queue_pop(&vm, d, q, &c) == 1);
    CHECK(q->last_avail == 0);
    queue_complete(&vm, d, q, &c, 1);
    CHECK(q->next_used == 0);
    destroy(&vm);
}

static void test_transport(void)
{
    struct vm vm;
    init(&vm);
    struct device *d = &vm.dev[0];
    struct virtq *q = setup_queue(&vm, 0, 0);
    CHECK(virtio_read(&vm, d, 0, 4) == 0x74726976);
    CHECK(virtio_read(&vm, d, 4, 4) == 2);
    virtio_write(&vm, d, 0x70, 4, 0);
    CHECK(!d->status && !q->ready);
    virtio_write(&vm, d, 0x24, 4, 1);
    virtio_write(&vm, d, 0x20, 4, 1);
    virtio_write(&vm, d, 0x70, 4, 11);
    CHECK(d->status == 11);
    virtio_write(&vm, d, 0x70, 4, 15);
    CHECK(d->status == 15);
    virtio_write(&vm, d, 0x70, 4, 0);
    virtio_write(&vm, d, 0x70, 4, 11);
    CHECK(!(d->status & 8));
    virtio_write(&vm, d, 0x70, 4, 7);
    CHECK(d->status & 64);
    d->config_len = 8;
    st64(d->config, UINT64_C(0xabcdef0123456789));
    CHECK(virtio_read(&vm, d, 0x100, 8) == UINT64_C(0xabcdef0123456789));
    CHECK(virtio_read(&vm, d, 0x107, 2) == 0);
    virtio_write(&vm, d, 0x30, 4, UINT32_MAX);
    CHECK(virtio_read(&vm, d, 0x34, 4) == 0);
    destroy(&vm);
}

static void test_completion_irq(void)
{
    struct vm vm;
    init(&vm);
    struct device *d = &vm.dev[0];
    struct virtq *q = setup_queue(&vm, 0, 0);
    struct chain c;
    desc(&vm, q, 0, q->desc + 0x3000, 8, 2, 0);
    unsigned initial = interrupts;
    for (unsigned i = 0; i < 2; ++i) {
        post(&vm, q, 0);
        CHECK(queue_pop(&vm, d, q, &c) == 1);
        queue_complete(&vm, d, q, &c, 8);
        CHECK(d->interrupt == 1 && interrupts == initial + 1);
    }
    virtio_write(&vm, d, 0x064, 4, 1);
    CHECK(d->interrupt == 0);
    post(&vm, q, 0);
    CHECK(queue_pop(&vm, d, q, &c) == 1);
    queue_complete(&vm, d, q, &c, 8);
    CHECK(d->interrupt == 1 && interrupts == initial + 2);
    virtio_write(&vm, d, 0x064, 4, 1);
    st16(guest_ptr(&vm.mem, q->avail, 2), 1);
    post(&vm, q, 0);
    CHECK(queue_pop(&vm, d, q, &c) == 1);
    queue_complete(&vm, d, q, &c, 8);
    CHECK(d->interrupt == 0 && interrupts == initial + 2);
    CHECK(q->next_used == 4);
    destroy(&vm);
}

static void test_mmio_wake(void)
{
    struct vm vm;
    init(&vm);
    vm.wake = test_wake;
    unsigned initial_wakes = wakeups, initial_irqs = irq_calls;
    uint64_t value = 'x';
    CHECK(mmio_access(&vm, UART_BASE, 4, true, &value));
    CHECK(mmio_access(&vm, UART_BASE, 4, true, &value));
    CHECK(irq_calls == initial_irqs && wakeups == initial_wakes);
    value = 0x20;
    CHECK(mmio_access(&vm, UART_BASE + 0x38, 4, true, &value));
    CHECK(irq_calls == initial_irqs + 1 && vm.uart_irq);
    CHECK(mmio_access(&vm, UART_BASE + 0x38, 4, true, &value));
    value = 'y';
    CHECK(mmio_access(&vm, UART_BASE, 4, true, &value));
    CHECK(irq_calls == initial_irqs + 1);
    value = 0;
    CHECK(mmio_access(&vm, UART_BASE + 0x38, 4, true, &value));
    CHECK(!vm.uart_irq && irq_calls == initial_irqs + 2);
    CHECK(mmio_access(&vm, UART_BASE + 0x38, 4, true, &value));
    CHECK(irq_calls == initial_irqs + 2 && wakeups == initial_wakes);

    struct device *rng = &vm.dev[3];
    rng->id = 4;
    struct virtq *q = setup_queue(&vm, 3, 0);
    desc(&vm, q, 0, q->desc + 0x3000, 32, 2, 0);
    post(&vm, q, 0);
    value = 0;
    CHECK(mmio_access(&vm, VIRTIO_BASE + 3 * VIRTIO_STRIDE + 0x50,
                     4, true, &value));
    CHECK(q->next_used == 1 && wakeups == initial_wakes);
    virtio_write(&vm, rng, 0x14, 4, 1);
    virtio_write(&vm, rng, 0x30, 4, 0);
    CHECK(wakeups == initial_wakes);
    initial_irqs = irq_calls;
    virtio_write(&vm, rng, 0x64, 4, 0);
    CHECK(irq_calls == initial_irqs && rng->interrupt == 1);
    virtio_write(&vm, rng, 0x64, 4, 1);
    CHECK(irq_calls == initial_irqs + 1 && !rng->interrupt);
    virtio_write(&vm, rng, 0x64, 4, 1);
    CHECK(irq_calls == initial_irqs + 1 && wakeups == initial_wakes);

    struct device *vsock = &vm.dev[4];
    vsock->id = 19;
    vsock->queue_count = 3;
    virtio_write(&vm, vsock, 0x14, 4, 1);
    virtio_write(&vm, vsock, 0x64, 4, 0);
    CHECK(wakeups == initial_wakes);
    virtio_write(&vm, vsock, 0x50, 4, 0);
    CHECK(wakeups == initial_wakes + 1);
    virtio_write(&vm, vsock, 0x70, 4, 0);
    CHECK(wakeups == initial_wakes + 2);
    destroy(&vm);
}

static unsigned reclaim_calls;
static uint64_t reclaim_start;
static size_t reclaim_size;

static int test_reclaim(struct vm *vm, uint64_t gpa, size_t len)
{
    reclaim_calls++;
    reclaim_start = gpa;
    reclaim_size = len;
    void *p = guest_ptr(&vm->mem, gpa, len);
    CHECK(p != NULL);
    memset(p, 0, len);
    return 0;
}

static void test_reporting(void)
{
    struct vm vm;
    init(&vm);
    vm.reclaim = test_reclaim;
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    uint64_t start = RAM_BASE + 0x100000;
    memset(guest_ptr(&vm.mem, start, page * 4), 0x5a, page * 4);
    reclaim_calls = 0;
    CHECK(!memory_report_free(&vm, start + 4096, 4096));
    if (page == 16384) {
        CHECK(reclaim_calls == 0);
        CHECK(*(uint8_t *)guest_ptr(&vm.mem, start + 4096, 1) == 0x5a);
    }
    CHECK(!memory_report_free(&vm, start + 4096, 2 * page));
    CHECK(reclaim_start == start + page && reclaim_size == page);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, start, 1) == 0x5a);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, start + page, 1) == 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, start + 2 * page, 1) == 0x5a);
    CHECK(memory_report_free(&vm, UINT64_MAX - 4096, 8192) == -1);
    CHECK(memory_report_free(&vm, RAM_BASE + vm.mem.size - 4096, 8192) == -1);

    /* Linux lends a reporting chain using writable, page-sized descriptors. */
    struct device *d = &vm.dev[5];
    d->id = 5;
    d->driver_features |= 1u << 5;
    struct virtq *q = setup_queue(&vm, 5, 2);
    desc(&vm, q, 0, start, (uint32_t)page, 2, 0);
    post(&vm, q, 0);
    device_notify(&vm, d, 2);
    CHECK(ld16(guest_ptr(&vm.mem, q->used + 2, 2)) == 1);
    CHECK(reclaim_start == start && reclaim_size == page);
    CHECK(!(d->status & 64));
    destroy(&vm);
}

static void block_chain(struct vm *vm, struct virtq *q, uint32_t op,
                         uint64_t sector, bool write)
{
    uint64_t header = q->desc + 0x3000, data = q->desc + 0x3100;
    uint64_t status = q->desc + 0x3400;
    st32(guest_ptr(&vm->mem, header, 16), op);
    st64(guest_ptr(&vm->mem, header + 8, 8), sector);
    *(uint8_t *)guest_ptr(&vm->mem, status, 1) = 0xff;
    desc(vm, q, 0, header, 16, 1, 1);
    if (op == 4)
        desc(vm, q, 1, status, 1, 2, 0);
    else {
        desc(vm, q, 1, data, 512, write ? 1 : 3, 2);
        desc(vm, q, 2, status, 1, 2, 0);
    }
    post(vm, q, 0);
}

static void test_block(void)
{
    struct vm vm;
    init(&vm);
    struct device *d = &vm.dev[0];
    d->id = 2;
    char path[] = "build/block-test-XXXXXX";
    d->disk_fd = mkstemp(path);
    CHECK(d->disk_fd >= 0);
    CHECK(!unlink(path));
    CHECK(!fcntl(d->disk_fd, F_NOCACHE, 1));
    CHECK(!ftruncate(d->disk_fd, 4096));
    d->disk_size = 4096;
    struct virtq *q = setup_queue(&vm, 0, 0);
    memset(guest_ptr(&vm.mem, q->desc + 0x3100, 512), 0x5a, 512);
    block_chain(&vm, q, 1, 2, true);
    device_notify(&vm, d, 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3400, 1) == 0);
    uint8_t buf[512];
    CHECK(pread(d->disk_fd, buf, sizeof(buf), 1024) == sizeof(buf));
    for (unsigned i = 0; i < sizeof(buf); ++i)
        CHECK(buf[i] == 0x5a);
    q = setup_queue(&vm, 0, 0);
    block_chain(&vm, q, 0, 2, false);
    device_notify(&vm, d, 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3400, 1) == 0);
    CHECK(!memcmp(guest_ptr(&vm.mem, q->desc + 0x3100, 512), buf, 512));
    CHECK(ld32(guest_ptr(&vm.mem, q->used + 8, 4)) == 513);
    q = setup_queue(&vm, 0, 0);
    block_chain(&vm, q, 4, 0, false);
    device_notify(&vm, d, 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3400, 1) == 0);
    q = setup_queue(&vm, 0, 0);
    block_chain(&vm, q, 0, UINT64_MAX, false);
    device_notify(&vm, d, 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3400, 1) == 1);
    q = setup_queue(&vm, 0, 0);
    d->readonly = true;
    block_chain(&vm, q, 1, 0, true);
    device_notify(&vm, d, 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3400, 1) == 1);
    CHECK(!ftruncate(d->disk_fd, 1025));
    d->file_size = 1025;
    d->disk_size = 1536;
    uint8_t tail = 0xaa;
    CHECK(pwrite(d->disk_fd, &tail, 1, 1024) == 1);
    q = setup_queue(&vm, 0, 0);
    block_chain(&vm, q, 0, 2, false);
    device_notify(&vm, d, 0);
    uint8_t *tail_sector = guest_ptr(&vm.mem, q->desc + 0x3100, 512);
    CHECK(tail_sector[0] == 0xaa && tail_sector[1] == 0 && tail_sector[511] == 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3400, 1) == 0);
    d->readonly = false;
    d->features |= 1u << 13;
    d->discard_alignment = (uint32_t)sysconf(_SC_PAGESIZE);
    size_t page = d->discard_alignment;
    d->disk_size = page * 8;
    CHECK(!ftruncate(d->disk_fd, (off_t)d->disk_size));
    uint8_t *filled = malloc((size_t)d->disk_size);
    CHECK(filled != NULL);
    memset(filled, 0xa5, (size_t)d->disk_size);
    CHECK(pwrite(d->disk_fd, filled, (size_t)d->disk_size, 0) == (ssize_t)d->disk_size);
    CHECK(!fsync(d->disk_fd));
    struct stat before, after;
    CHECK(!fstat(d->disk_fd, &before));
    q = setup_queue(&vm, 0, 0);
    uint64_t haddr = q->desc + 0x3000, ranges = q->desc + 0x3100;
    uint64_t status = q->desc + 0x3400;
    st32(guest_ptr(&vm.mem, haddr, 16), 11);
    /* Start inside a host page: preserve both boundary pages. */
    st64(guest_ptr(&vm.mem, ranges, 16), 1);
    st32(guest_ptr(&vm.mem, ranges + 8, 4), (uint32_t)(6 * page / 512));
    desc(&vm, q, 0, haddr, 16, 1, 1);
    desc(&vm, q, 1, ranges, 16, 1, 2);
    desc(&vm, q, 2, status, 1, 2, 0);
    post(&vm, q, 0);
    device_notify(&vm, d, 0);
    uint8_t discard_status = *(uint8_t *)guest_ptr(&vm.mem, status, 1);
    CHECK(discard_status == 0 || discard_status == 2);
    CHECK(!fsync(d->disk_fd));
    CHECK(pread(d->disk_fd, filled, (size_t)d->disk_size, 0) == (ssize_t)d->disk_size);
    if (!discard_status) {
        CHECK(!fstat(d->disk_fd, &after));
        CHECK(after.st_blocks < before.st_blocks);
        for (size_t i = 0; i < d->disk_size; ++i)
            CHECK(filled[i] == (i >= page && i < page * 6 ? 0 : 0xa5));
    } else {
        puts("NOTE: host filesystem does not support F_PUNCHHOLE");
    }
    /* A later invalid range must prevent the earlier valid range being punched. */
    memset(filled, 0x5a, (size_t)d->disk_size);
    CHECK(pwrite(d->disk_fd, filled, (size_t)d->disk_size, 0) == (ssize_t)d->disk_size);
    q = setup_queue(&vm, 0, 0);
    st32(guest_ptr(&vm.mem, haddr, 16), 11);
    st64(guest_ptr(&vm.mem, ranges, 16), 0);
    st32(guest_ptr(&vm.mem, ranges + 8, 4), (uint32_t)(page / 512));
    st64(guest_ptr(&vm.mem, ranges + 16, 16), UINT64_MAX);
    st32(guest_ptr(&vm.mem, ranges + 24, 4), 1);
    desc(&vm, q, 0, haddr, 16, 1, 1);
    desc(&vm, q, 1, ranges, 32, 1, 2);
    desc(&vm, q, 2, status, 1, 2, 0);
    post(&vm, q, 0);
    device_notify(&vm, d, 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, status, 1) == 1);
    CHECK(pread(d->disk_fd, buf, sizeof(buf), 0) == sizeof(buf));
    CHECK(buf[0] == 0x5a);
    free(filled);
    destroy(&vm);
}

static struct virtq *large_block_chain(struct vm *vm, unsigned segments,
                                       uint32_t segment_size, bool write,
                                       bool merged_header)
{
    struct virtq *q = setup_queue(vm, 1, 0);
    q->num = QUEUE_MAX;
    uint64_t header = q->desc + 0x3000, status = header + 0x100;
    uint64_t payload = RAM_BASE + 0x400000;
    st32(guest_ptr(&vm->mem, header, 16), write ? 1 : 0);
    st64(guest_ptr(&vm->mem, header + 8, 8), 1);
    *(uint8_t *)guest_ptr(&vm->mem, status, 1) = 0xff;
    unsigned first = merged_header ? 0 : 1;
    if (merged_header) {
        memcpy(guest_ptr(&vm->mem, payload - 16, 16),
               guest_ptr(&vm->mem, header, 16), 16);
    } else {
        desc(vm, q, 0, header, 16, 1, 1);
    }
    for (unsigned i = 0; i < segments; ++i) {
        uint64_t addr = payload + (uint64_t)i * segment_size;
        uint32_t bytes = segment_size;
        if (!i && merged_header) {
            addr -= 16;
            bytes += 16;
        }
        desc(vm, q, first + i, addr, bytes, write ? 1 : 3,
             (uint16_t)(first + i + 1));
    }
    desc(vm, q, first + segments, status, 1, 2, 0);
    post(vm, q, 0);
    return q;
}

static void test_block_limits(void)
{
    struct vm vm;
    init(&vm);
    char root[] = "build/block-root-XXXXXX", data[] = "build/block-data-XXXXXX";
    int root_fd = mkstemp(root), data_fd = mkstemp(data);
    CHECK(root_fd >= 0 && data_fd >= 0);
    CHECK(!ftruncate(root_fd, 1025));
    const uint8_t tail = 0x93;
    CHECK(pwrite(root_fd, &tail, 1, 1024) == 1);
    CHECK(!ftruncate(data_fd, BLOCK_DATA_MAX + 512));
    CHECK(!close(root_fd) && !close(data_fd));
    vm.opt.root = root;
    vm.opt.data = data;
    CHECK(!devices_init(&vm));
    struct device *d = &vm.dev[1];
    uint32_t size_max = (uint32_t)virtio_read(&vm, d, 0x108, 4);
    uint32_t seg_max = (uint32_t)virtio_read(&vm, d, 0x10c, 4);
    CHECK(size_max == BLOCK_SEGMENT_MAX && seg_max == BLOCK_SEGMENTS_MAX);
    CHECK((d->features & 6) == 6 && seg_max + 2 <= QUEUE_MAX);
    struct virtq *root_queue = setup_queue(&vm, 0, 0);
    block_chain(&vm, root_queue, 0, 2, false);
    device_notify(&vm, &vm.dev[0], 0);
    uint8_t *root_tail = guest_ptr(&vm.mem, root_queue->desc + 0x3100, 512);
    CHECK(root_tail[0] == tail && root_tail[1] == 0 && root_tail[511] == 0);
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, root_queue->desc + 0x3400, 1) == 0);
    uint64_t payload = RAM_BASE + 0x400000;
    uint8_t *bytes = guest_ptr(&vm.mem, payload, BLOCK_DATA_MAX);
    uint8_t expected[BLOCK_SEGMENT_MAX];
    for (size_t i = 0; i < sizeof(expected); ++i)
        expected[i] = (uint8_t)(i * 37 + i / 251);
    for (unsigned trial = 0; trial < 3; ++trial) {
        /* First cover Linux's 1 MiB payload plus its separate header/status. */
        unsigned segments = trial == 0 ? MAX_TRANSFER / size_max : seg_max;
        size_t length = (size_t)segments * size_max;
        for (unsigned i = 0; i < segments; ++i)
            memcpy(bytes + (size_t)i * size_max, expected, size_max);
        struct virtq *q = large_block_chain(&vm, segments, size_max, true,
                                            trial == 2);
        device_notify(&vm, d, 0);
        CHECK(!(d->status & 64));
        CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3100, 1) == 0);
        CHECK(ld16(guest_ptr(&vm.mem, q->used + 2, 2)) == 1);
        CHECK(ld32(guest_ptr(&vm.mem, q->used + 8, 4)) == 1);
        /* FLUSH and an independent descriptor must observe all written bytes. */
        q = setup_queue(&vm, 1, 0);
        block_chain(&vm, q, 4, 0, false);
        device_notify(&vm, d, 0);
        CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3400, 1) == 0);
        int verify_fd = open(data, O_RDONLY);
        CHECK(verify_fd >= 0);
        uint8_t actual[BLOCK_SEGMENT_MAX];
        for (unsigned i = 0; i < segments; ++i) {
            CHECK(pread(verify_fd, actual, sizeof(actual),
                         512 + (off_t)i * size_max) == sizeof(actual));
            CHECK(!memcmp(actual, expected, sizeof(actual)));
        }
        CHECK(!close(verify_fd));
        memset(bytes, 0, length);
        q = large_block_chain(&vm, segments, size_max, false, false);
        device_notify(&vm, d, 0);
        CHECK(!(d->status & 64));
        CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3100, 1) == 0);
        CHECK(ld32(guest_ptr(&vm.mem, q->used + 8, 4)) == length + 1);
        for (unsigned i = 0; i < segments; ++i)
            CHECK(!memcmp(bytes + (size_t)i * size_max, expected, size_max));
    }
    /* Exceeding either advertised limit returns IOERR without poisoning vdb. */
    struct virtq *q = large_block_chain(&vm, 1, size_max + 512, false, false);
    device_notify(&vm, d, 0);
    CHECK(!(d->status & 64));
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3100, 1) == 1);
    q = large_block_chain(&vm, seg_max + 1, 512, false, false);
    device_notify(&vm, d, 0);
    CHECK(!(d->status & 64));
    CHECK(*(uint8_t *)guest_ptr(&vm.mem, q->desc + 0x3100, 1) == 1);
    CHECK(ld32(guest_ptr(&vm.mem, q->used + 8, 4)) == 1);
    q = large_block_chain(&vm, 1, BLOCK_DATA_MAX + 512, false, false);
    device_notify(&vm, d, 0);
    CHECK(d->status & 64);
    destroy(&vm);
    CHECK(!unlink(root) && !unlink(data));
}

static void test_net_rng(void)
{
    struct vm vm;
    init(&vm);
    int fds[2];
    CHECK(!socketpair(AF_UNIX, SOCK_DGRAM, 0, fds));
    CHECK(!fd_nonblock(fds[0]));
    CHECK(!fd_nonblock(fds[1]));
    vm.opt.net_fd = fds[0];
    struct device *d = &vm.dev[2];
    d->id = 1;
    struct virtq *q = setup_queue(&vm, 2, 0);
    desc(&vm, q, 0, q->desc + 0x3000, 256, 2, 0);
    post(&vm, q, 0);
    uint8_t frame[64], got[256];
    memset(frame, 0xaa, sizeof(frame));
    CHECK(send(fds[1], frame, sizeof(frame), 0) == sizeof(frame));
    net_receive(&vm);
    CHECK(vm.net_rx == 1);
    CHECK(ld32(guest_ptr(&vm.mem, q->used + 8, 4)) == sizeof(frame) + 12);
    CHECK(!memcmp(guest_ptr(&vm.mem, q->desc + 0x300c, 64), frame, 64));
    q = setup_queue(&vm, 2, 1);
    desc(&vm, q, 0, q->desc + 0x3000, sizeof(frame) + 12, 0, 0);
    memcpy(guest_ptr(&vm.mem, q->desc + 0x300c, 64), frame, 64);
    post(&vm, q, 0);
    device_notify(&vm, d, 1);
    CHECK(recv(fds[1], got, sizeof(got), 0) == sizeof(frame));
    CHECK(!memcmp(got, frame, 64) && vm.net_tx == 1);
    q = setup_queue(&vm, 3, 0);
    vm.dev[3].id = 4;
    desc(&vm, q, 0, q->desc + 0x3000, 256, 2, 0);
    post(&vm, q, 0);
    device_notify(&vm, &vm.dev[3], 0);
    uint8_t zeros[256] = {0};
    CHECK(memcmp(guest_ptr(&vm.mem, q->desc + 0x3000, 256), zeros, 256));
    CHECK(ld32(guest_ptr(&vm.mem, q->used + 8, 4)) == 256);
    close(fds[0]);
    close(fds[1]);
    destroy(&vm);
}

static uint32_t big32(const void *p) { return __builtin_bswap32(ld32(p)); }

static void test_rtc(void)
{
    struct vm vm;
    init(&vm);
    CHECK(!rtc_init(&vm));
    uint32_t before = (uint32_t)time(NULL);
    rtc_write(&vm, 0x0c, 1);
    uint64_t value = 0;
    CHECK(mmio_access(&vm, RTC_BASE, 4, false, &value));
    CHECK(value >= before - 1 && value <= (uint32_t)time(NULL) + 1);
    CHECK(mmio_access(&vm, RTC_BASE + 0xfe0, 4, false, &value) && value == 0x31);
    CHECK(mmio_access(&vm, RTC_BASE + 0xfe8, 4, false, &value) && value == 0x04);
    CHECK(!mmio_access(&vm, RTC_BASE, 8, false, &value));
    CHECK(rtc_read(&vm, 0x0c) == 1 && rtc_read(&vm, 0x10) == 0);
    CHECK(rtc_timeout(&vm) == -1);
    rtc_write(&vm, 0x08, 1234567890);
    CHECK(rtc_read(&vm, 0x00) - 1234567890u <= 1);
    CHECK(rtc_read(&vm, 0x08) == 1234567890);
    CHECK((uint32_t)time(NULL) >= before && (uint32_t)time(NULL) - before < 2);
    rtc_write(&vm, 0x00, 0);
    CHECK(rtc_read(&vm, 0x00) - 1234567890u <= 1);
    rtc_write(&vm, 0x0c, 1);
    CHECK(rtc_read(&vm, 0x00) <= 1);
    rtc_write(&vm, 0x04, rtc_read(&vm, 0x00));
    CHECK(rtc_read(&vm, 0x14) == 1 && rtc_read(&vm, 0x18) == 0);
    unsigned old_interrupts = interrupts;
    rtc_write(&vm, 0x10, 1);
    CHECK(rtc_read(&vm, 0x18) == 1 && interrupts > old_interrupts);
    rtc_write(&vm, 0x1c, 1);
    CHECK(rtc_read(&vm, 0x14) == 0 && rtc_read(&vm, 0x18) == 0);
    CHECK(rtc_timeout(&vm) > 0);
    rtc_write(&vm, 0x0c, 0);
    CHECK(rtc_read(&vm, 0x00) == 0 && rtc_read(&vm, 0x0c) == 0);
    CHECK(rtc_timeout(&vm) == -1);
    destroy(&vm);
}

static void test_fdt(void)
{
    struct vm vm;
    init(&vm);
    vm.opt.cpus = 4;
    vm.gic_dist_size = 65536;
    vm.gic_redist_size = 64 * 0x20000;
    vm.boot.initrd_start = RAM_BASE + 0x800000;
    vm.boot.initrd_end = vm.boot.initrd_start + 0x1234;
    uint8_t buf[65536];
    size_t size;
    for (unsigned initialize = 0; initialize < 2; ++initialize) {
        vm.opt.initialize_data_disk = initialize != 0;
        CHECK(!fdt_build(&vm, buf, sizeof(buf), &size));
        CHECK(big32(buf) == 0xd00dfeed && big32(buf + 4) == size);
        CHECK(big32(buf + 20) == 17 && big32(buf + 24) == 16);
        uint32_t offset = big32(buf + 8), nameoff = big32(buf + 12);
        unsigned depth = 0, cpus = 0, virtio = 0, initrd = 0, memory = 0;
        unsigned rtc = 0, data_init = 0;
        while (offset < nameoff) {
            uint32_t tok = big32(buf + offset);
            offset += 4;
            if (tok == 1) {
                const char *name = (char *)buf + offset;
                if (!strncmp(name, "cpu@", 4)) cpus++;
                if (!strncmp(name, "virtio_mmio@", 12)) virtio++;
                if (!strcmp(name, "rtc@9010000")) rtc++;
                depth++;
                offset += (uint32_t)((strlen(name) + 4) & ~(size_t)3);
            } else if (tok == 2) {
                CHECK(depth > 0);
                depth--;
            } else if (tok == 3) {
                uint32_t len = big32(buf + offset), noff = big32(buf + offset + 4);
                offset += 8;
                CHECK(noff < size - nameoff && offset + len <= nameoff);
                const char *name = (char *)buf + nameoff + noff;
                if (!strcmp(name, "linux,initrd-start")) {
                    CHECK(len == 8 && big32(buf + offset + 4) == (uint32_t)vm.boot.initrd_start);
                    initrd++;
                }
                if (!strcmp(name, "device_type") && !strcmp((char *)buf + offset, "memory"))
                    memory++;
                if (!strcmp(name, "compatible") &&
                    !strcmp((char *)buf + offset, "arm,pl031"))
                    rtc++;
                if (!strcmp(name, "sapphire,initialize-data-disk")) {
                    CHECK(len == 0 && depth == 2);
                    data_init++;
                }
                offset += (len + 3) & ~3u;
            } else if (tok == 9) {
                CHECK(depth == 0 && offset == nameoff);
            } else {
                CHECK(false);
            }
        }
        CHECK(cpus == 4 && virtio == DEVICE_COUNT && initrd == 1 && memory == 1);
        CHECK(rtc == 2 && data_init == initialize);
    }
    CHECK(fdt_build(&vm, buf, 8, &size) == -1);
    destroy(&vm);
}

static struct chain guest_packet(struct vsock_header *h, const void *data)
{
    struct chain c = {.count = 2, .readable = sizeof(*h) + h->len};
    c.seg[0] = (struct segment){.data = (uint8_t *)h, .len = sizeof(*h)};
    c.seg[1] = (struct segment){.data = (uint8_t *)data, .len = h->len};
    return c;
}

static void test_vsock(void)
{
    struct vm vm;
    init(&vm);
    vm.kq = kqueue();
    CHECK(vm.kq >= 0);
    int fds[2];
    CHECK(!socketpair(AF_UNIX, SOCK_STREAM, 0, fds));
    CHECK(!fd_nonblock(fds[0]) && !fd_nonblock(fds[1]));
    vm.dev[4].id = 19;
    struct vsock_client *c = &vm.clients[0];
    c->fd = fds[0];
    c->state = 1;
    CHECK(send(fds[1], "CONNECT 1024\nhello", 18, 0) == 18);
    vsock_event(&vm, c->fd, true, false, 0);
    CHECK(c->state == 2 && c->guest_port == 1024);
    CHECK(vm.packets && vm.packets->hdr.op == 1);
    uint32_t host_port = c->port;
    CHECK(host_port >= 0x40000000);
    struct virtq *q = setup_queue(&vm, 4, 0);
    desc(&vm, q, 0, q->desc + 0x3000, 1024, 2, 0);
    post(&vm, q, 0);
    vsock_drain(&vm);
    CHECK(!vm.packets);
    struct vsock_header h = {.src_cid = 3, .dst_cid = 2, .src_port = 1024,
        .dst_port = host_port, .type = 1, .op = 2, .buf_alloc = 4096};
    struct chain chain = guest_packet(&h, NULL);
    vsock_tx(&vm, &chain);
    CHECK(c->state == 3);
    vsock_event(&vm, c->fd, false, true, 0);
    char buf[64];
    CHECK(recv(fds[1], buf, sizeof(buf), 0) == 3 && !memcmp(buf, "OK\n", 3));
    vsock_event(&vm, c->fd, true, false, 0);
    CHECK(vm.packets && vm.packets->hdr.op == 5 && vm.packets->hdr.len == 5);
    CHECK(!memcmp(vm.packets->data, "hello", 5));
    CHECK(c->tx_count == 5);
    post(&vm, q, 0);
    vsock_drain(&vm);
    CHECK(!vm.packets);
    CHECK(!memcmp(guest_ptr(&vm.mem, q->desc + 0x3000 + 44, 5), "hello", 5));
    h.op = 5;
    h.len = 5;
    h.fwd_cnt = 5;
    chain = guest_packet(&h, "world");
    vsock_tx(&vm, &chain);
    CHECK(c->output_len == 5);
    vsock_event(&vm, c->fd, false, true, 0);
    CHECK(recv(fds[1], buf, sizeof(buf), 0) == 5 && !memcmp(buf, "world", 5));
    CHECK(c->fwd_count == 5);
    CHECK(vm.packets && vm.packets->hdr.op == 6);
    /* An impossible forward count must reset just this stream. */
    h.op = 6;
    h.len = 0;
    h.fwd_cnt = 6;
    chain = guest_packet(&h, NULL);
    vsock_tx(&vm, &chain);
    CHECK(c->fd == -1 && vm.packets && vm.packets->hdr.op == 3);
    close(fds[1]);
    destroy(&vm);
}

static int pump_vsock(struct vm *vm)
{
    vsock_update_events(vm);
    struct kevent events[16];
    const struct timespec zero = {0};
    int count = kevent(vm->kq, NULL, 0, events, 16, &zero);
    CHECK(count >= 0);
    for (int i = 0; i < count; ++i)
        vsock_event(vm, (int)events[i].ident, events[i].filter == EVFILT_READ,
                     events[i].filter == EVFILT_WRITE, events[i].flags);
    CHECK(!atomic_load(&vm->stop));
    return count;
}

static void connected_vsock(struct vm *vm, int fds[2])
{
    CHECK(!socketpair(AF_UNIX, SOCK_STREAM, 0, fds));
    CHECK(!fd_nonblock(fds[0]) && !fd_nonblock(fds[1]));
    struct vsock_client *c = &vm->clients[0];
    CHECK(c->fd == -1);
    *c = (struct vsock_client){.fd = fds[0], .state = 3, .ack_offset = 3,
        .port = 2000, .guest_port = 1024, .output = malloc(VSOCK_BUFFER)};
    CHECK(c->output != NULL);
}

static void test_vsock_eof(void)
{
    struct vm vm;
    init(&vm);
    vm.kq = kqueue();
    CHECK(vm.kq >= 0);
    vm.dev[4].id = 19;
    int fds[2];
    struct vsock_client *c = &vm.clients[0];
    for (unsigned trial = 0; trial < 3; ++trial) {
        connected_vsock(&vm, fds);
        CHECK(pump_vsock(&vm) > 0);
        CHECK(pump_vsock(&vm) == 0);
        CHECK(send(fds[1], "blocked", 7, 0) == 7);
        CHECK(pump_vsock(&vm) > 0 && !c->tx_count && !vm.packets);
        CHECK(pump_vsock(&vm) == 0); /* Buffered data at zero credit cannot spin. */
        if (trial == 1) {
            struct vsock_header h = {.src_cid = 3, .dst_cid = 2,
                .src_port = 1024, .dst_port = c->port, .type = 1,
                .op = 4, .flags = 2};
            struct chain chain = guest_packet(&h, NULL);
            vsock_tx(&vm, &chain);
            CHECK(c->write_shutdown);
            (void)pump_vsock(&vm);
            CHECK(c->fd == fds[0] && pump_vsock(&vm) == 0);
        }
        if (trial == 2) {
            memset(c->output, 0xa7, VSOCK_BUFFER);
            c->output_len = VSOCK_BUFFER;
        }
        CHECK(!close(fds[1]));
        for (unsigned i = 0; i < 4 && c->fd >= 0; ++i)
            CHECK(pump_vsock(&vm) > 0);
        CHECK(c->fd == -1 && c->output == NULL);
        CHECK(fcntl(fds[0], F_GETFD) < 0 && errno == EBADF);
        CHECK(vm.packets && vm.packets->hdr.op == 3 && !vm.packets->next);
        vsock_reset(&vm);
        CHECK(pump_vsock(&vm) == 0);
    }

    connected_vsock(&vm, fds);
    CHECK(pump_vsock(&vm) > 0 && pump_vsock(&vm) == 0);
    CHECK(!shutdown(fds[1], SHUT_WR));
    CHECK(pump_vsock(&vm) > 0 && c->host_eof && c->fd >= 0);
    CHECK(vm.packets && vm.packets->hdr.op == 4 && vm.packets->hdr.flags == 2);
    (void)pump_vsock(&vm);
    CHECK(pump_vsock(&vm) == 0);
    CHECK(!close(fds[1]));
    CHECK(pump_vsock(&vm) > 0 && c->fd == -1);
    vsock_reset(&vm);

    connected_vsock(&vm, fds);
    int small = 4096;
    CHECK(!setsockopt(fds[0], SOL_SOCKET, SO_SNDBUF, &small, sizeof(small)));
    for (unsigned i = 0; i < VSOCK_BUFFER; ++i)
        c->output[i] = (uint8_t)(i * 31 + i / 197);
    c->output_len = VSOCK_BUFFER;
    CHECK(send(fds[1], "request", 7, 0) == 7);
    CHECK(!shutdown(fds[1], SHUT_WR));
    for (unsigned i = 0; i < 4; ++i)
        (void)pump_vsock(&vm);
    CHECK(c->fd >= 0 && c->read_eof && !c->host_eof);
    CHECK(!c->tx_count && c->output_len && c->output_len < VSOCK_BUFFER);
    CHECK(pump_vsock(&vm) == 0);

    struct vsock_header h = {.src_cid = 3, .dst_cid = 2,
        .src_port = 1024, .dst_port = c->port, .type = 1,
        .op = 6, .buf_alloc = 7};
    struct chain chain = guest_packet(&h, NULL);
    vsock_tx(&vm, &chain);
    for (unsigned i = 0; i < 4 && !c->host_eof; ++i)
        CHECK(pump_vsock(&vm) > 0);
    CHECK(c->host_eof && c->tx_count == 7 && c->output_len);
    struct vsock_packet *p = vm.packets;
    while (p && p->hdr.op != 5)
        p = p->next;
    CHECK(p && p->hdr.len == 7 && !memcmp(p->data, "request", 7));
    CHECK(p->next && p->next->hdr.op == 4 && p->next->hdr.flags == 2);

    h.op = 4;
    h.flags = 2;
    chain = guest_packet(&h, NULL);
    vsock_tx(&vm, &chain);
    CHECK(!c->write_shutdown && c->output_len);
    uint8_t received[VSOCK_BUFFER];
    size_t total = 0;
    bool eof = false;
    for (unsigned tries = 0; tries < 256 && !eof; ++tries) {
        uint8_t part[8192];
        ssize_t n = recv(fds[1], part, sizeof(part), 0);
        if (n > 0) {
            CHECK(total + (size_t)n <= sizeof(received));
            memcpy(received + total, part, (size_t)n);
            total += (size_t)n;
        } else if (!n) {
            eof = true;
        } else {
            CHECK(errno == EAGAIN || errno == EWOULDBLOCK);
        }
        (void)pump_vsock(&vm);
    }
    CHECK(eof && total == sizeof(received) && c->fd == -1);
    for (unsigned i = 0; i < sizeof(received); ++i)
        CHECK(received[i] == (uint8_t)(i * 31 + i / 197));
    CHECK(pump_vsock(&vm) == 0);
    CHECK(!close(fds[1]));
    destroy(&vm);
}

static void test_fuzz(void)
{
    struct vm vm;
    init(&vm);
    struct chain c;
    for (unsigned n = 0; n < 10000; ++n) {
        struct virtq *q = setup_queue(&vm, 0, 0);
        uint8_t *table = guest_ptr(&vm.mem, q->desc, 128);
        arc4random_buf(table, 128);
        for (unsigned i = 0; i < 8; ++i) {
            if (arc4random_uniform(2))
                st64(table + i * 16, q->desc + 0x3000 + i * 64);
            if (arc4random_uniform(2))
                st32(table + i * 16 + 8, arc4random_uniform(128));
            st16(table + i * 16 + 12, (uint16_t)arc4random_uniform(8));
            st16(table + i * 16 + 14, (uint16_t)arc4random_uniform(10));
        }
        post(&vm, q, (uint16_t)arc4random_uniform(10));
        int rc = queue_pop(&vm, &vm.dev[0], q, &c);
        CHECK(rc == 1 || rc == -1);
        if (rc == 1) {
            CHECK(c.count > 0 && c.count <= 8);
            queue_complete(&vm, &vm.dev[0], q, &c, c.writable);
        }
    }
    destroy(&vm);
}

static void test_reset_policy(void)
{
    struct reset_policy policy = {0};
    CHECK(reset_allowed(&policy, 0));
    CHECK(reset_allowed(&policy, RESET_STABLE_MS - 1));
    CHECK(!reset_allowed(&policy, 1));
    CHECK(!reset_allowed(&policy, 0));
    CHECK(policy.rapid_resets == RAPID_RESET_LIMIT);
    CHECK(reset_allowed(&policy, RESET_STABLE_MS));
    CHECK(policy.rapid_resets == 0);
    CHECK(reset_allowed(&policy, 0));
    CHECK(reset_allowed(&policy, 0));
    CHECK(!reset_allowed(&policy, 0));
    CHECK(reset_allowed(&policy, RESET_STABLE_MS + 1));
    CHECK(policy.rapid_resets == 0);
}

int main(void)
{
    verbose = getenv("VERBOSE") != NULL;
    test_bounds();
    test_transport();
    test_completion_irq();
    test_mmio_wake();
    test_reporting();
    test_block();
    test_block_limits();
    test_net_rng();
    test_rtc();
    test_fdt();
    test_vsock();
    test_vsock_eof();
    test_fuzz();
    test_reset_policy();
    printf("PASS: %u assertions; bounds, rings, transport, block, net, rng, RTC, FDT, "
           "vsock EOF/credit/drain, MMIO wake/IRQ transitions, host-page-safe reporting "
           "and 10000 malformed-ring cases (%u IRQ assertions)\n",
           assertions, interrupts);
    return 0;
}
