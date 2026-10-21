#include "vmm.h"

#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/file.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <unistd.h>

#define VERSION_1 (UINT64_C(1) << 32)

static int disk_open(struct device *d, const char *path, bool readonly)
{
    struct stat st;
    d->disk_fd = open(path, (readonly ? O_RDONLY : O_RDWR) | O_CLOEXEC);
    if (d->disk_fd < 0)
        return -1;
    if (fstat(d->disk_fd, &st) || !S_ISREG(st.st_mode) ||
        st.st_size < 512 || (!readonly && (st.st_size & 511))) {
        errno = EINVAL;
        return -1;
    }
    if (flock(d->disk_fd, (readonly ? LOCK_SH : LOCK_EX) | LOCK_NB))
        return -1;
    /* Linux already caches disk contents; retain FLUSH's fsync durability. */
    if (fcntl(d->disk_fd, F_NOCACHE, 1))
        vm_log("disk %s: F_NOCACHE unavailable: %s; using cached I/O",
               path, strerror(errno));
    d->readonly = readonly;
    d->file_size = (uint64_t)st.st_size;
    d->disk_size = ((uint64_t)st.st_size + (readonly ? 511 : 0)) & ~UINT64_C(511);
    st64(d->config, d->disk_size / 512);
    st32(d->config + 8, BLOCK_SEGMENT_MAX);
    st32(d->config + 12, BLOCK_SEGMENTS_MAX);
    st32(d->config + 20, 512);
    d->config_len = 24;
    d->features |= (1u << 1) | (1u << 2) | (1u << 6) | (1u << 9);
    if (readonly) {
        d->features |= 1u << 5;
    } else {
        size_t alignment = (size_t)sysconf(_SC_PAGESIZE);
        if ((size_t)st.st_blksize > alignment)
            alignment = (size_t)st.st_blksize;
        if (alignment >= 512 && !(alignment & (alignment - 1)) &&
            alignment <= 1024u * 1024u) {
            d->discard_alignment = (uint32_t)alignment;
            d->features |= 1u << 13;
            st32(d->config + 36, DISCARD_MAX_SECTORS);
            st32(d->config + 40, DISCARD_MAX_RANGES);
            st32(d->config + 44, (uint32_t)alignment / 512);
            d->config_len = 60;
        }
    }
    return 0;
}

int devices_init(struct vm *vm)
{
    vsock_init(vm);
    static const uint32_t ids[DEVICE_COUNT] = {2, 2, 1, 4, 19, 5};
    for (unsigned i = 0; i < DEVICE_COUNT; ++i) {
        struct device *d = &vm->dev[i];
        d->id = ids[i];
        d->index = i;
        d->irq = VIRTIO_IRQ + i;
        d->features = VERSION_1;
        d->queue_count = (d->id == 5 || d->id == 19) ? 3 : d->id == 1 ? 2 : 1;
        d->disk_fd = -1;
    }
    if (disk_open(&vm->dev[0], vm->opt.root, true) ||
        disk_open(&vm->dev[1], vm->opt.data, false)) {
        vm_log("open/lock disk: %s", strerror(errno));
        return -1;
    }
    struct device *net = &vm->dev[2];
    net->features |= (1u << 5) | (1u << 16);
    const uint8_t mac[] = {0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xee};
    memcpy(net->config, mac, sizeof(mac));
    st16(net->config + 6, 1);
    net->config_len = 8;
    st64(vm->dev[4].config, 3);
    vm->dev[4].config_len = 8;
    /* No inflation target; Linux's reporting queue lends free pages temporarily. */
    vm->dev[5].features |= (1u << 0) | (1u << 5);
    vm->dev[5].config_len = 8;
    vm->uart_cr = 0x301;
    return rtc_init(vm);
}

void devices_destroy(struct vm *vm)
{
    vsock_destroy(vm);
    for (unsigned i = 0; i < DEVICE_COUNT; ++i) {
        if (vm->dev[i].disk_fd >= 0) {
            if (!vm->dev[i].readonly)
                (void)fsync(vm->dev[i].disk_fd);
            close(vm->dev[i].disk_fd);
            vm->dev[i].disk_fd = -1;
        }
    }
}

uint64_t virtio_read(struct vm *vm, struct device *d, uint32_t off, unsigned size)
{
    (void)vm;
    if (off >= 0x100) {
        uint64_t result = 0;
        if (size <= sizeof(result) && off - 0x100 <= d->config_len &&
            size <= d->config_len - (off - 0x100))
            memcpy(&result, d->config + (off - 0x100), size);
        return result;
    }
    if (size != 4 || (off & 3))
        return 0;
    struct virtq *q = d->queue_sel < d->queue_count ?
                      &d->queue[d->queue_sel] : NULL;
    switch (off) {
    case 0x000: return 0x74726976;
    case 0x004: return 2;
    case 0x008: return d->id;
    case 0x00c: return 0x53415050;
    case 0x010: return d->feature_sel < 2 ?
                       (uint32_t)(d->features >> (32 * d->feature_sel)) : 0;
    case 0x034: return q ? QUEUE_MAX : 0;
    case 0x044: return q && q->ready;
    case 0x060: return d->interrupt;
    case 0x070: return d->status;
    case 0x0fc: return 0;
    default: return 0;
    }
}

void virtio_write(struct vm *vm, struct device *d, uint32_t off, unsigned size,
                  uint64_t value)
{
    if (d->id == 5 && off == 0x104 && size == 4) {
        st32(d->config + 4, (uint32_t)value);
        return;
    }
    if (size != 4 || (off & 3))
        return;
    uint32_t v = (uint32_t)value;
    bool wake = false;
    struct virtq *q = d->queue_sel < d->queue_count ?
                      &d->queue[d->queue_sel] : NULL;
    switch (off) {
    case 0x014: d->feature_sel = v; break;
    case 0x020:
        if (d->driver_sel < 2 && !(d->status & 8)) {
            unsigned shift = 32 * d->driver_sel;
            d->driver_features = (d->driver_features & ~(UINT64_C(0xffffffff) << shift)) |
                                 ((uint64_t)v << shift);
        }
        break;
    case 0x024: d->driver_sel = v; break;
    case 0x030: d->queue_sel = v; break;
    case 0x038: if (q && !q->ready) q->num = v; break;
    case 0x044:
        if (!q)
            break;
        if (!v) {
            q->ready = false;
        } else if (v == 1 && !q->ready) {
            if (!queue_valid(vm, q))
                device_fail(vm, d, "invalid queue configuration");
            else {
                q->last_avail = q->next_used = 0;
                q->ready = true;
                wake = d->id == 19;
            }
        }
        break;
    case 0x050:
        if (v < d->queue_count && (d->status & 4) && !(d->status & (64 | 128))) {
            device_notify(vm, d, v);
            wake = d->id == 19;
        }
        break;
    case 0x064: {
        bool was_pending = d->interrupt != 0;
        d->interrupt &= ~(v & 3);
        if (was_pending != (d->interrupt != 0) && vm->set_irq)
            vm->set_irq(vm, d->irq, d->interrupt != 0);
        break;
    }
    case 0x070:
        wake = d->id == 19;
        if (!v) {
            d->status = d->interrupt = 0;
            d->driver_features = 0;
            d->feature_sel = d->driver_sel = d->queue_sel = 0;
            memset(d->queue, 0, sizeof(d->queue));
            if (d->id == 19)
                vsock_reset(vm);
            if (vm->set_irq)
                vm->set_irq(vm, d->irq, false);
        } else {
            if ((v & 8) && ((d->driver_features & ~d->features) ||
                           !(d->driver_features & VERSION_1)))
                v &= ~8u;
            d->status = v | (d->status & 64);
            if ((v & 4) && !(v & 8))
                device_fail(vm, d, "DRIVER_OK without negotiated VERSION_1");
        }
        break;
    case 0x080: if (q && !q->ready) q->desc = (q->desc & ~UINT64_C(0xffffffff)) | v; break;
    case 0x084: if (q && !q->ready) q->desc = (q->desc & UINT64_C(0xffffffff)) | ((uint64_t)v << 32); break;
    case 0x090: if (q && !q->ready) q->avail = (q->avail & ~UINT64_C(0xffffffff)) | v; break;
    case 0x094: if (q && !q->ready) q->avail = (q->avail & UINT64_C(0xffffffff)) | ((uint64_t)v << 32); break;
    case 0x0a0: if (q && !q->ready) q->used = (q->used & ~UINT64_C(0xffffffff)) | v; break;
    case 0x0a4: if (q && !q->ready) q->used = (q->used & UINT64_C(0xffffffff)) | ((uint64_t)v << 32); break;
    default: break;
    }
    if (wake && vm->wake)
        vm->wake(vm);
}

static int disk_io(int fd, void *buf, size_t len, uint64_t offset, bool write)
{
    size_t done = 0;
    while (done < len) {
        ssize_t n = write ?
            pwrite(fd, (uint8_t *)buf + done, len - done, (off_t)(offset + done)) :
            pread(fd, (uint8_t *)buf + done, len - done, (off_t)(offset + done));
        if (n < 0 && errno == EINTR)
            continue;
        if (n <= 0)
            return -1;
        done += (size_t)n;
    }
    return 0;
}

static uint8_t disk_discard(struct device *d, const struct chain *c)
{
    if (d->readonly || !(d->features & (1u << 13)) || c->writable != 1 ||
        c->readable <= 16 || ((c->readable - 16) % 16))
        return 1;
    unsigned count = (c->readable - 16) / 16;
    if (count > DISCARD_MAX_RANGES)
        return 1;
    struct { uint64_t start, size; } ranges[DISCARD_MAX_RANGES];
    /* Validate the entire request before changing any part of the backing file. */
    for (unsigned i = 0; i < count; ++i) {
        uint8_t raw[16];
        if (chain_read(c, 16 + 16 * i, raw, sizeof(raw)) != sizeof(raw))
            return 1;
        uint64_t sector = ld64(raw);
        uint32_t sectors = ld32(raw + 8);
        if (ld32(raw + 12) || sectors > DISCARD_MAX_SECTORS ||
            sector > d->disk_size / 512 ||
            (uint64_t)sectors * 512 > d->disk_size - sector * 512)
            return 1;
        uint64_t start = sector * 512, end = start + (uint64_t)sectors * 512;
        uint64_t mask = d->discard_alignment - 1;
        uint64_t aligned_start = (start + mask) & ~mask;
        uint64_t aligned_end = end & ~mask;
        ranges[i].start = aligned_start;
        ranges[i].size = aligned_end > aligned_start ? aligned_end - aligned_start : 0;
    }
    for (unsigned i = 0; i < count; ++i) {
        if (!ranges[i].size)
            continue;
        fpunchhole_t hole = {.fp_offset = (off_t)ranges[i].start,
                            .fp_length = (off_t)ranges[i].size};
        if (fcntl(d->disk_fd, F_PUNCHHOLE, &hole))
            return errno == ENOTSUP || errno == EOPNOTSUPP ? 2 : 1;
    }
    return 0;
}

static uint32_t block_request(struct device *d, struct chain *c)
{
    uint8_t header[16], status = 1;
    uint32_t written = 0;
    if (c->count < 2 || c->seg[0].write || c->seg[0].len < 16 ||
        !c->seg[c->count - 1].write || c->seg[c->count - 1].len != 1 ||
        chain_read(c, 0, header, sizeof(header)) != sizeof(header))
        return 0;
    uint32_t type = ld32(header);
    uint64_t sector = ld64(header + 8);
    bool write = type == 1;
    size_t len = write ? c->readable - 16 : c->writable - 1;
    if (type == 0 || type == 1) {
        if (len > BLOCK_DATA_MAX || (len & 511) ||
            (write && (d->readonly || c->writable != 1)) ||
            (!write && c->readable != 16) ||
            sector > d->disk_size / 512 ||
            len > d->disk_size - sector * 512)
            goto done;
        unsigned segments = 0;
        for (unsigned i = 0; i + 1 < c->count; ++i) {
            /* The header and status are outside the advertised data limits. */
            uint32_t bytes = c->seg[i].len - (i == 0 ? 16u : 0u);
            if (bytes > BLOCK_SEGMENT_MAX ||
                ((i || bytes) && ++segments > BLOCK_SEGMENTS_MAX))
                goto done;
        }
        uint8_t buf[16384];
        for (size_t done = 0; done < len;) {
            size_t n = len - done;
            if (n > sizeof(buf))
                n = sizeof(buf);
            if (write && chain_read(c, 16 + done, buf, n) != n)
                goto done;
            uint64_t offset = sector * 512 + done;
            size_t io_size = n;
            if (!write && d->readonly && d->file_size &&
                (offset >= d->file_size || io_size > d->file_size - offset)) {
                memset(buf, 0, n);
                io_size = offset < d->file_size ? (size_t)(d->file_size - offset) : 0;
            }
            if (disk_io(d->disk_fd, buf, io_size, offset, write))
                goto done;
            if (!write) {
                if (chain_write(c, done, buf, n) != n)
                    goto done;
                written += (uint32_t)n;
            }
            done += n;
        }
        status = 0;
    } else if (type == 4) {
        if (c->readable == 16 && c->writable == 1 &&
            (d->readonly || fsync(d->disk_fd) == 0))
            status = 0;
    } else if (type == 8) {
        const char id[20] = "sapphire-virtio-blk";
        if (c->readable == 16 && c->writable >= sizeof(id) + 1) {
            written = (uint32_t)chain_write(c, 0, id, sizeof(id));
            status = 0;
        }
    } else if (type == 11) {
        status = disk_discard(d, c);
    } else {
        status = 2;
    }
done:
    *c->seg[c->count - 1].data = status;
    return written + 1;
}

static void net_transmit(struct vm *vm, const struct chain *c)
{
    uint8_t packet[65536], header[12];
    if (c->writable || c->readable < sizeof(header) + 14 ||
        c->readable > sizeof(packet) + sizeof(header) ||
        chain_read(c, 0, header, sizeof(header)) != sizeof(header) ||
        header[0] || header[1]) {
        vm->net_drop++;
        return;
    }
    size_t len = c->readable - sizeof(header);
    chain_read(c, sizeof(header), packet, len);
    ssize_t n;
    do {
        n = send(vm->opt.net_fd, packet, len, MSG_DONTWAIT | MSG_NOSIGNAL);
    } while (n < 0 && errno == EINTR);
    if (n == (ssize_t)len)
        vm->net_tx++;
    else {
        vm->net_drop++;
        if (n < 0 && (errno == EPIPE || errno == ENOTCONN || errno == ECONNRESET))
            vm_request_stop(vm, 1);
    }
}

void net_receive(struct vm *vm)
{
    uint8_t packet[65536], header[12] = {0};
    struct device *d = &vm->dev[2];
    for (unsigned budget = 0; budget < QUEUE_MAX; ++budget) {
        ssize_t n = recv(vm->opt.net_fd, packet, sizeof(packet),
                         MSG_DONTWAIT | MSG_TRUNC);
        if (n < 0 && errno == EINTR) {
            --budget;
            continue;
        }
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK))
            return;
        if (n <= 0) {
            vm->net_drop++;
            vm_log("network peer terminated: %s", n ? strerror(errno) : "EOF");
            vm_request_stop(vm, 1);
            return;
        }
        struct chain c;
        if ((size_t)n > sizeof(packet) || n < 14 ||
            queue_pop(vm, d, &d->queue[0], &c) <= 0) {
            vm->net_drop++;
            continue;
        }
        if (c.readable || c.writable < sizeof(header) + (size_t)n) {
            queue_complete(vm, d, &d->queue[0], &c, 0);
            vm->net_drop++;
            continue;
        }
        st16(header + 10, 1);
        chain_write(&c, 0, header, sizeof(header));
        chain_write(&c, sizeof(header), packet, (size_t)n);
        queue_complete(vm, d, &d->queue[0], &c, (uint32_t)(sizeof(header) + n));
        vm->net_rx++;
    }
}

void device_notify(struct vm *vm, struct device *d, unsigned qi)
{
    if ((d->id == 1 || d->id == 19) && qi == 0) {
        if (d->id == 19)
            vsock_drain(vm);
        return;
    }
    if (d->id == 19 && qi == 2)
        return; /* The transport never changes CID or initiates reset events. */
    struct virtq *q = &d->queue[qi];
    struct chain c;
    for (unsigned budget = 0; budget < QUEUE_MAX &&
         queue_pop(vm, d, q, &c) > 0; ++budget) {
        uint32_t written = 0;
        switch (d->id) {
        case 2:
            written = block_request(d, &c);
            if (!written) {
                device_fail(vm, d, "block request lacks a valid status descriptor");
                return;
            }
            break;
        case 1:
            net_transmit(vm, &c);
            break;
        case 4:
            if (c.readable) {
                device_fail(vm, d, "rng queue must contain writable buffers");
                return;
            }
            for (unsigned i = 0; i < c.count; ++i)
                arc4random_buf(c.seg[i].data, c.seg[i].len);
            written = c.writable;
            break;
        case 19:
            vsock_tx(vm, &c);
            break;
        case 5:
            /* Optional stats and hint queues are absent, so reporting is #2. */
            if (qi == 2) {
                if (!(d->driver_features & (1u << 5)) || c.readable) {
                    device_fail(vm, d, "invalid balloon reporting queue");
                    return;
                }
                for (unsigned i = 0; i < c.count; ++i) {
                    if (memory_report_free(vm, c.seg[i].gpa, c.seg[i].len)) {
                        device_fail(vm, d, "free-page reclamation failed");
                        vm_request_stop(vm, 3);
                        return;
                    }
                }
            } else if (qi > 1) {
                device_fail(vm, d, "unnegotiated balloon queue");
                return;
            }
            /* Inflate/deflate are acknowledged; num_pages remains zero. */
            break;
        default: break;
        }
        queue_complete(vm, d, q, &c, written);
    }
    if (d->id == 19)
        vsock_drain(vm);
}

void console_flush(struct vm *vm)
{
    size_t off = 0;
    while (off < vm->console_len) {
        ssize_t n = write(vm->console_fd, vm->console_buf + off,
                           vm->console_len - off);
        if (n < 0 && errno == EINTR)
            continue;
        if (n <= 0)
            break;
        off += (size_t)n;
    }
    vm->console_len = 0;
}

static uint32_t uart_read(struct vm *vm, uint32_t off)
{
    static const uint8_t ids[8] = {0x11, 0x10, 0x14, 0x00, 0x0d, 0xf0, 0x05, 0xb1};
    if (off >= 0xfe0 && off <= 0xffc && !(off & 3))
        return ids[(off - 0xfe0) / 4];
    switch (off) {
    case 0x018: return 0x90; /* TX empty, RX empty; no input source. */
    case 0x024: return vm->uart_ibrd;
    case 0x028: return vm->uart_fbrd;
    case 0x02c: return vm->uart_lcr;
    case 0x030: return vm->uart_cr;
    case 0x038: return vm->uart_imsc;
    case 0x03c: return 0x20;
    case 0x040: return vm->uart_imsc & 0x20;
    default: return 0;
    }
}

static void uart_write(struct vm *vm, uint32_t off, uint32_t v)
{
    switch (off) {
    case 0:
        vm->console_buf[vm->console_len++] = (char)v;
        if ((char)v == '\n' || vm->console_len == sizeof(vm->console_buf))
            console_flush(vm);
        break;
    case 0x024: vm->uart_ibrd = v; break;
    case 0x028: vm->uart_fbrd = v; break;
    case 0x02c: vm->uart_lcr = v; break;
    case 0x030: vm->uart_cr = v; break;
    case 0x038: vm->uart_imsc = v & 0x7ff; break;
    default: break;
    }
    bool level = (vm->uart_imsc & 0x20) != 0;
    if (level != vm->uart_irq) {
        vm->uart_irq = level;
        if (vm->set_irq)
            vm->set_irq(vm, UART_IRQ, level);
    }
}

bool mmio_access(struct vm *vm, uint64_t addr, unsigned size, bool write,
                 uint64_t *value)
{
    if ((size != 1 && size != 2 && size != 4 && size != 8) ||
        (addr & (size - 1)))
        return false;
    bool handled = true;
    pthread_mutex_lock(&vm->io_lock);
    if (addr >= UART_BASE && addr <= UART_BASE + 0x1000 - size) {
        if (size > 4)
            handled = false;
        else if (write)
            uart_write(vm, (uint32_t)(addr - UART_BASE), (uint32_t)*value);
        else
            *value = uart_read(vm, (uint32_t)(addr - UART_BASE));
    } else if (addr >= RTC_BASE && addr <= RTC_BASE + 0x1000 - size) {
        if (size != 4)
            handled = false;
        else if (write)
            rtc_write(vm, (uint32_t)(addr - RTC_BASE), (uint32_t)*value);
        else
            *value = rtc_read(vm, (uint32_t)(addr - RTC_BASE));
    } else if (addr >= VIRTIO_BASE &&
               addr < VIRTIO_BASE + DEVICE_COUNT * VIRTIO_STRIDE) {
        unsigned index = (unsigned)((addr - VIRTIO_BASE) / VIRTIO_STRIDE);
        uint32_t off = (uint32_t)((addr - VIRTIO_BASE) % VIRTIO_STRIDE);
        if (size > VIRTIO_STRIDE - off)
            handled = false;
        else if (write)
            virtio_write(vm, &vm->dev[index], off, size, *value);
        else
            *value = virtio_read(vm, &vm->dev[index], off, size);
    } else {
        handled = false;
    }
    pthread_mutex_unlock(&vm->io_lock);
    return handled;
}
