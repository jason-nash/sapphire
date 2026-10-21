#ifndef SAPPHIRE_VMM_H
#define SAPPHIRE_VMM_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdatomic.h>
#include <pthread.h>
#include <sys/types.h>

#define RAM_BASE UINT64_C(0x40000000)
#define GIC_DIST UINT64_C(0x08000000)
#define GIC_REDIST UINT64_C(0x10000000)
#define UART_BASE UINT64_C(0x09000000)
#define RTC_BASE UINT64_C(0x09010000)
#define VIRTIO_BASE UINT64_C(0x0a000000)
#define VIRTIO_STRIDE 0x1000u
#define UART_IRQ 33u
#define RTC_IRQ 34u
#define VIRTIO_IRQ 48u
#define DEVICE_COUNT 6u
#define MAX_CPUS 256u
#define QUEUE_MAX 256u
#define QUEUES_MAX 3u
#define CHAIN_MAX QUEUE_MAX
#define MAX_TRANSFER (1024u * 1024u)
#define BLOCK_SEGMENT_MAX (64u * 1024u)
#define BLOCK_SEGMENTS_MAX 128u
#define BLOCK_DATA_MAX (BLOCK_SEGMENT_MAX * BLOCK_SEGMENTS_MAX)
#define BLOCK_CHAIN_MAX (BLOCK_DATA_MAX + 16u + 1u)
#define DISCARD_MAX_SECTORS 65536u
#define DISCARD_MAX_RANGES 32u
#define VSOCK_CLIENTS 64u
#define VSOCK_BUFFER 65536u
#define VSOCK_PACKET_MAX 16384u
#define VSOCK_BACKLOG (1024u * 1024u)
#define RAPID_RESET_LIMIT 3u
#define RESET_STABLE_MS UINT64_C(30000)

struct reset_policy {
    unsigned rapid_resets;
};

struct memory {
    uint8_t *ram;
    size_t size;
};

struct boot {
    uint64_t entry, dtb, initrd_start, initrd_end, kernel_end;
    size_t dtb_size;
};

struct options {
    const char *kernel, *initrd, *root, *data, *vsock, *console;
    size_t memory_mib;
    unsigned cpus;
    int net_fd, parent_fd;
    bool probe, initialize_data_disk, memory_diagnostics;
};

struct virtq {
    uint64_t desc, avail, used;
    uint32_t num;
    uint16_t last_avail, next_used;
    bool ready;
};

struct segment {
    uint8_t *data;
    uint64_t gpa;
    uint32_t len;
    bool write;
};

struct chain {
    uint16_t head, count;
    uint32_t readable, writable;
    struct segment seg[CHAIN_MAX];
};

struct device {
    uint32_t id, index, irq;
    uint32_t feature_sel, driver_sel, queue_sel, status, interrupt;
    uint64_t features, driver_features;
    struct virtq queue[QUEUES_MAX];
    unsigned queue_count;
    uint8_t config[64];
    size_t config_len;
    int disk_fd;
    uint64_t disk_size, file_size;
    uint32_t discard_alignment;
    bool readonly;
};

struct vsock_header {
    uint64_t src_cid, dst_cid;
    uint32_t src_port, dst_port, len;
    uint16_t type, op;
    uint32_t flags, buf_alloc, fwd_cnt;
} __attribute__((packed));

struct vsock_client {
    int fd;
    unsigned state;
    uint32_t port, guest_port, peer_alloc, peer_fwd, tx_count;
    uint32_t recv_count, fwd_count;
    uint32_t shutdown;
    char command[64];
    size_t command_len, ack_offset;
    uint8_t *output;
    size_t output_len;
    uint64_t deadline_ms;
    bool read_enabled, write_enabled, host_eof, credit_pending;
    bool read_watched, write_watched, read_eof, write_shutdown;
    bool close_after_output;
};

struct vsock_packet {
    struct vsock_packet *next;
    struct vsock_header hdr;
    size_t offset;
    uint8_t data[];
};

struct vm;
typedef void (*irq_callback)(struct vm *, unsigned, bool);
typedef void (*wake_callback)(struct vm *);
typedef int (*reclaim_callback)(struct vm *, uint64_t, size_t);

struct vm {
    struct options opt;
    struct memory mem;
    struct boot boot;
    struct device dev[DEVICE_COUNT];
    pthread_mutex_t io_lock;
    atomic_int stop; /* 0 running, 1 power off/signal, 2 reset, 3 fatal */
    irq_callback set_irq;
    wake_callback wake;
    reclaim_callback reclaim;
    void *hv;
    int kq, listener, console_fd;
    bool socket_owned;
    dev_t socket_dev;
    ino_t socket_ino;
    struct vsock_client clients[VSOCK_CLIENTS];
    uint32_t next_port;
    struct vsock_packet *packets, **packet_tail;
    size_t packet_bytes;
    uint32_t uart_imsc, uart_cr, uart_lcr, uart_ibrd, uart_fbrd;
    bool uart_irq;
    uint32_t rtc_match, rtc_load, rtc_control, rtc_imsc, rtc_raw;
    int64_t rtc_offset;
    uint64_t rtc_deadline_ms;
    bool rtc_match_set;
    char console_buf[4096];
    size_t console_len;
    uint64_t net_rx, net_tx, net_drop, vsock_rx, vsock_tx;
    uint64_t reported_free_bytes, reclaimed_bytes, report_skipped_bytes;
    size_t gic_dist_size, gic_redist_size;
};

uint16_t ld16(const void *);
uint32_t ld32(const void *);
uint64_t ld64(const void *);
void st16(void *, uint16_t);
void st32(void *, uint32_t);
void st64(void *, uint64_t);
void *guest_ptr(const struct memory *, uint64_t, size_t);
int memory_create(struct memory *, size_t);
void memory_destroy(struct memory *);
int boot_load(struct vm *);
int memory_report_free(struct vm *, uint64_t, size_t);
uint64_t memory_snapshot(const struct memory *, const char *);
int fdt_build(struct vm *, void *, size_t, size_t *);

bool queue_valid(struct vm *, const struct virtq *);
int queue_pop(struct vm *, struct device *, struct virtq *, struct chain *);
void queue_complete(struct vm *, struct device *, struct virtq *,
                    const struct chain *, uint32_t);
size_t chain_read(const struct chain *, size_t, void *, size_t);
size_t chain_write(const struct chain *, size_t, const void *, size_t);
void device_fail(struct vm *, struct device *, const char *);
int devices_init(struct vm *);
void devices_destroy(struct vm *);
uint64_t virtio_read(struct vm *, struct device *, uint32_t, unsigned);
void virtio_write(struct vm *, struct device *, uint32_t, unsigned, uint64_t);
void device_notify(struct vm *, struct device *, unsigned);
void net_receive(struct vm *);
bool mmio_access(struct vm *, uint64_t, unsigned, bool, uint64_t *);
void console_flush(struct vm *);
int rtc_init(struct vm *);
uint32_t rtc_read(struct vm *, uint32_t);
void rtc_write(struct vm *, uint32_t, uint32_t);
int rtc_timeout(struct vm *);

void vsock_init(struct vm *);
void vsock_destroy(struct vm *);
void vsock_reset(struct vm *);
void vsock_tx(struct vm *, const struct chain *);
void vsock_drain(struct vm *);
void vsock_accept(struct vm *);
void vsock_event(struct vm *, int, bool, bool, uint16_t);
void vsock_update_events(struct vm *);
int vsock_timeout(struct vm *);
uint64_t monotonic_ms(void);

int io_init(struct vm *);
int io_run(struct vm *);
void io_destroy(struct vm *);
void io_wake(struct vm *);
int fd_nonblock(int);
int hv_start(struct vm *);
void hv_stop(struct vm *);
int hv_probe(unsigned);
int hv_memory_reclaim(struct vm *, uint64_t, size_t);
void vm_request_stop(struct vm *, int);
void vm_log(const char *, ...) __attribute__((format(printf, 1, 2)));
bool reset_allowed(struct reset_policy *, uint64_t);

#endif
