#include "vmm.h"

#include <errno.h>
#include <limits.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

enum { COMMAND = 1, CONNECTING, ESTABLISHED };
enum { REQUEST = 1, RESPONSE, RESET, SHUTDOWN, RW, CREDIT_UPDATE, CREDIT_REQUEST };

_Static_assert(sizeof(struct vsock_header) == 44, "virtio vsock wire header");

uint64_t monotonic_ms(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static struct vsock_header host_header(const struct vsock_client *c, uint16_t op)
{
    return (struct vsock_header){.src_cid = 2, .dst_cid = 3,
        .src_port = c->port, .dst_port = c->guest_port, .type = 1, .op = op,
        .buf_alloc = VSOCK_BUFFER, .fwd_cnt = c->fwd_count};
}

static bool packet_queue(struct vm *vm, struct vsock_header h, const void *data)
{
    size_t size = sizeof(struct vsock_packet) + h.len;
    if (size > VSOCK_BACKLOG - vm->packet_bytes) {
        vm_log("vsock outbound queue exhausted");
        vm_request_stop(vm, 3);
        return false;
    }
    struct vsock_packet *p = malloc(size);
    if (!p) {
        vm_request_stop(vm, 3);
        return false;
    }
    p->next = NULL;
    p->hdr = h;
    p->offset = 0;
    if (h.len)
        memcpy(p->data, data, h.len);
    *vm->packet_tail = p;
    vm->packet_tail = &p->next;
    vm->packet_bytes += size;
    return true;
}

static void purge_port(struct vm *vm, uint32_t port)
{
    struct vsock_packet **p = &vm->packets;
    while (*p) {
        struct vsock_packet *packet = *p;
        if (packet->hdr.src_port == port) {
            *p = packet->next;
            vm->packet_bytes -= sizeof(*packet) + packet->hdr.len;
            free(packet);
        } else {
            p = &packet->next;
        }
    }
    vm->packet_tail = p;
}

static void client_close(struct vm *vm, struct vsock_client *c, bool reset)
{
    struct vsock_header h = host_header(c, RESET);
    bool connected = c->state >= CONNECTING;
    purge_port(vm, c->port);
    if (c->fd >= 0)
        close(c->fd);
    free(c->output);
    memset(c, 0, sizeof(*c));
    c->fd = -1;
    if (reset && connected)
        packet_queue(vm, h, NULL);
}

void vsock_init(struct vm *vm)
{
    vm->packet_tail = &vm->packets;
    vm->next_port = 0x40000000;
    for (unsigned i = 0; i < VSOCK_CLIENTS; ++i)
        vm->clients[i].fd = -1;
}

void vsock_reset(struct vm *vm)
{
    for (unsigned i = 0; i < VSOCK_CLIENTS; ++i)
        client_close(vm, &vm->clients[i], false);
    while (vm->packets) {
        struct vsock_packet *p = vm->packets;
        vm->packets = p->next;
        free(p);
    }
    vm->packet_tail = &vm->packets;
    vm->packet_bytes = 0;
}

void vsock_destroy(struct vm *vm) { vsock_reset(vm); }

static struct vsock_client *find_port(struct vm *vm, uint32_t port)
{
    for (unsigned i = 0; i < VSOCK_CLIENTS; ++i)
        if (vm->clients[i].state >= CONNECTING && vm->clients[i].port == port)
            return &vm->clients[i];
    return NULL;
}

static void shutdown_output(struct vsock_client *c)
{
    if (!c->write_shutdown) {
        (void)shutdown(c->fd, SHUT_WR);
        c->write_shutdown = true;
    }
}

void vsock_tx(struct vm *vm, const struct chain *chain)
{
    struct vsock_header h;
    if (chain->writable || chain->readable < sizeof(h) ||
        chain_read(chain, 0, &h, sizeof(h)) != sizeof(h) ||
        h.src_cid != 3 || h.dst_cid != 2 || h.type != 1 ||
        h.len != chain->readable - sizeof(h))
        return;
    struct vsock_client *c = find_port(vm, h.dst_port);
    if (!c || c->guest_port != h.src_port) {
        if (h.op != RESET) {
            struct vsock_header rst = {.src_cid = 2, .dst_cid = 3,
                .src_port = h.dst_port, .dst_port = h.src_port, .type = 1,
                .op = RESET, .buf_alloc = VSOCK_BUFFER};
            packet_queue(vm, rst, NULL);
        }
        return;
    }
    if (h.op == RESET) {
        client_close(vm, c, false);
        return;
    }
    if ((h.op != RW && h.len) ||
        (uint32_t)(h.fwd_cnt - c->peer_fwd) >
        (uint32_t)(c->tx_count - c->peer_fwd)) {
        client_close(vm, c, true);
        return;
    }
    c->peer_alloc = h.buf_alloc;
    c->peer_fwd = h.fwd_cnt;
    switch (h.op) {
    case RESPONSE:
        if (c->state != CONNECTING)
            goto invalid;
        c->state = ESTABLISHED;
        c->deadline_ms = 0;
        break;
    case RW:
        if (c->state != ESTABLISHED || (c->shutdown & 2) ||
            h.len > VSOCK_BUFFER - c->output_len ||
            chain_read(chain, sizeof(h), c->output + c->output_len, h.len) != h.len)
            goto invalid;
        c->output_len += h.len;
        c->recv_count += h.len;
        vm->vsock_rx += h.len;
        break;
    case CREDIT_REQUEST:
        c->credit_pending = true;
        break;
    case CREDIT_UPDATE:
        break;
    case SHUTDOWN:
        if (c->state != ESTABLISHED || !h.flags || (h.flags & ~3u))
            goto invalid;
        c->shutdown |= h.flags;
        c->close_after_output = (c->shutdown == 3);
        if ((c->shutdown & 2) && !c->output_len && c->ack_offset == 3)
            shutdown_output(c);
        break;
    default:
        goto invalid;
    }
    return;
invalid:
    client_close(vm, c, true);
}

void vsock_drain(struct vm *vm)
{
    struct device *d = &vm->dev[4];
    for (unsigned i = 0; i < VSOCK_CLIENTS; ++i) {
        struct vsock_client *c = &vm->clients[i];
        if (c->credit_pending && c->state == ESTABLISHED) {
            c->credit_pending = false;
            if (!packet_queue(vm, host_header(c, CREDIT_UPDATE), NULL))
                return;
        }
    }
    struct chain chain;
    for (unsigned budget = 0; vm->packets && budget < QUEUE_MAX; ++budget) {
        if (queue_pop(vm, d, &d->queue[0], &chain) <= 0)
            return;
        struct vsock_packet *p = vm->packets;
        if (chain.readable || chain.writable < sizeof(p->hdr) ||
            (p->hdr.len > p->offset && chain.writable == sizeof(p->hdr))) {
            queue_complete(vm, d, &d->queue[0], &chain, 0);
            continue;
        }
        size_t n = p->hdr.len - p->offset;
        if (n > chain.writable - sizeof(p->hdr))
            n = chain.writable - sizeof(p->hdr);
        struct vsock_header h = p->hdr;
        h.len = (uint32_t)n;
        struct vsock_client *c = find_port(vm, h.src_port);
        if (c)
            h.fwd_cnt = c->fwd_count;
        chain_write(&chain, 0, &h, sizeof(h));
        if (n)
            chain_write(&chain, sizeof(h), p->data + p->offset, n);
        queue_complete(vm, d, &d->queue[0], &chain, (uint32_t)(sizeof(h) + n));
        p->offset += n;
        if (p->offset == p->hdr.len) {
            vm->packets = p->next;
            vm->packet_bytes -= sizeof(*p) + p->hdr.len;
            free(p);
            if (!vm->packets)
                vm->packet_tail = &vm->packets;
        }
    }
}

void vsock_accept(struct vm *vm)
{
    for (unsigned budget = 0; budget < VSOCK_CLIENTS; ++budget) {
        int fd = accept(vm->listener, NULL, NULL);
        if (fd < 0)
            return;
        if (fd_nonblock(fd)) {
            close(fd);
            continue;
        }
        struct vsock_client *c = NULL;
        for (unsigned i = 0; i < VSOCK_CLIENTS; ++i)
            if (vm->clients[i].fd < 0) {
                c = &vm->clients[i];
                break;
            }
        if (!c) {
            close(fd);
            continue;
        }
        memset(c, 0, sizeof(*c));
        c->fd = fd;
        c->state = COMMAND;
        c->deadline_ms = monotonic_ms() + 30000;
    }
}

static bool read_command(struct vm *vm, struct vsock_client *c)
{
    /* Reading a byte at a time leaves pipelined stream bytes in the socket. */
    for (unsigned budget = 0; budget < sizeof(c->command); ++budget) {
        char ch;
        ssize_t n = recv(c->fd, &ch, 1, MSG_DONTWAIT);
        if (n < 0 && errno == EINTR)
            continue;
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK))
            return true;
        if (n <= 0 || c->command_len >= sizeof(c->command) - 1)
            return false;
        c->command[c->command_len++] = ch;
        if (ch != '\n')
            continue;
        c->command[c->command_len - 1] = 0;
        if (strncmp(c->command, "CONNECT ", 8))
            return false;
        const char *s = c->command + 8;
        if (!*s)
            return false;
        uint64_t port = 0;
        for (; *s; ++s) {
            if (*s < '0' || *s > '9')
                return false;
            port = port * 10 + (unsigned)(*s - '0');
            if (port > UINT32_MAX)
                return false;
        }
        if (!port)
            return false;
        c->output = malloc(VSOCK_BUFFER);
        if (!c->output)
            return false;
        c->guest_port = (uint32_t)port;
        do {
            c->port = vm->next_port++;
        } while (!c->port || find_port(vm, c->port));
        c->state = CONNECTING;
        return packet_queue(vm, host_header(c, REQUEST), NULL);
    }
    return true;
}

static uint32_t credit(const struct vsock_client *c)
{
    uint32_t outstanding = c->tx_count - c->peer_fwd;
    return outstanding < c->peer_alloc ? c->peer_alloc - outstanding : 0;
}

static bool write_output(struct vm *vm, struct vsock_client *c)
{
    static const char ok[] = "OK\n";
    if (c->ack_offset < 3) {
        ssize_t n = send(c->fd, ok + c->ack_offset, 3 - c->ack_offset,
                         MSG_DONTWAIT | MSG_NOSIGNAL);
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR))
            return true;
        if (n <= 0)
            return false;
        c->ack_offset += (size_t)n;
        if (c->ack_offset < 3)
            return true;
    }
    if (c->output_len) {
        ssize_t n = send(c->fd, c->output, c->output_len,
                         MSG_DONTWAIT | MSG_NOSIGNAL);
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR))
            return true;
        if (n <= 0)
            return false;
        c->output_len -= (size_t)n;
        memmove(c->output, c->output + n, c->output_len);
        c->fwd_count += (uint32_t)n;
        c->credit_pending = true;
    }
    if (!c->output_len && (c->shutdown & 2))
        shutdown_output(c);
    if (!c->output_len && (c->close_after_output ||
                           (c->host_eof && (c->shutdown & 2))))
        return false;
    (void)vm;
    return true;
}

static void input_eof(struct vm *vm, struct vsock_client *c)
{
    if (!c->read_eof || c->host_eof || c->state != ESTABLISHED)
        return;
    uint8_t byte;
    ssize_t n;
    do {
        n = recv(c->fd, &byte, 1, MSG_PEEK | MSG_DONTWAIT);
    } while (n < 0 && errno == EINTR);
    if (n < 0 && errno != EAGAIN && errno != EWOULDBLOCK) {
        client_close(vm, c, true);
    } else if (!n) {
        c->host_eof = true;
        struct vsock_header h = host_header(c, SHUTDOWN);
        h.flags = 2;
        packet_queue(vm, h, NULL);
    }
}

void vsock_event(struct vm *vm, int fd, bool read, bool write, uint16_t flags)
{
    struct vsock_client *c = NULL;
    for (unsigned i = 0; i < VSOCK_CLIENTS; ++i)
        if (vm->clients[i].fd == fd) {
            c = &vm->clients[i];
            break;
        }
    if (!c)
        return;
    if (flags & EV_EOF) {
        struct sockaddr_storage peer;
        socklen_t size = sizeof(peer);
        if (getpeername(fd, (struct sockaddr *)&peer, &size)) {
            client_close(vm, c, true);
            vsock_drain(vm);
            return;
        }
    }
    if ((flags & EV_ERROR) ||
        (write && (flags & EV_EOF) && !c->write_shutdown)) {
        client_close(vm, c, true);
        vsock_drain(vm);
        return;
    }
    if (read && (flags & EV_EOF))
        c->read_eof = true;
    if (c->state == COMMAND && read) {
        if (!read_command(vm, c))
            client_close(vm, c, false);
    } else if (c->state == ESTABLISHED) {
        if (write && !(flags & EV_EOF) && !write_output(vm, c)) {
            client_close(vm, c, true);
            return;
        }
        if (read && !c->host_eof && !(c->shutdown & 1) && credit(c) &&
            vm->packet_bytes + sizeof(struct vsock_packet) + VSOCK_PACKET_MAX <= VSOCK_BACKLOG) {
            uint8_t data[VSOCK_PACKET_MAX];
            size_t len = credit(c);
            if (len > sizeof(data))
                len = sizeof(data);
            ssize_t n = recv(fd, data, len, MSG_DONTWAIT);
            if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK || errno == EINTR))
                return;
            if (n < 0) {
                client_close(vm, c, true);
                return;
            }
            struct vsock_header h = host_header(c, n ? RW : SHUTDOWN);
            h.len = (uint32_t)n;
            if (!n) {
                c->host_eof = true;
                h.flags = 2;
            } else {
                c->tx_count += (uint32_t)n;
                vm->vsock_tx += (uint64_t)n;
            }
            packet_queue(vm, h, data);
        }
    }
    if (c->fd >= 0) {
        input_eof(vm, c);
        if (c->fd >= 0 && c->host_eof && (c->shutdown & 2) && !c->output_len &&
            c->ack_offset == 3)
            client_close(vm, c, true);
    }
    vsock_drain(vm);
}

static void filter_set(struct vm *vm, int fd, int16_t filter, bool active,
                        bool watched)
{
    struct kevent ev[2];
    unsigned count = 0;
    /* Re-add when changing modes so EV_CLEAR cannot leak into a level watch. */
    if (watched)
        EV_SET(&ev[count++], fd, filter, EV_DELETE, 0, 0, NULL);
    EV_SET(&ev[count++], fd, filter, EV_ADD | EV_ENABLE |
           (active ? 0 : EV_CLEAR), 0, 0, NULL);
    if (kevent(vm->kq, ev, (int)count, NULL, 0, NULL) < 0) {
        vm_log("vsock kevent: %s", strerror(errno));
        vm_request_stop(vm, 3);
    }
}

void vsock_update_events(struct vm *vm)
{
    for (unsigned i = 0; i < VSOCK_CLIENTS; ++i) {
        struct vsock_client *c = &vm->clients[i];
        if (c->fd < 0)
            continue;
        bool read = c->state == COMMAND ||
            (c->state == ESTABLISHED && !c->host_eof && !(c->shutdown & 1) &&
             credit(c) && vm->packet_bytes + sizeof(struct vsock_packet) +
                         VSOCK_PACKET_MAX <= VSOCK_BACKLOG);
        bool write = c->state == ESTABLISHED &&
            (c->ack_offset < 3 || c->output_len || c->close_after_output);
        /* Keep EOF observable without repeated idle/read-credit wakeups. */
        if (!c->read_watched || read != c->read_enabled) {
            filter_set(vm, c->fd, EVFILT_READ, read, c->read_watched);
            c->read_enabled = read;
            c->read_watched = true;
        }
        if (!c->write_watched || write != c->write_enabled) {
            filter_set(vm, c->fd, EVFILT_WRITE, write, c->write_watched);
            c->write_enabled = write;
            c->write_watched = true;
        }
    }
}

int vsock_timeout(struct vm *vm)
{
    uint64_t now = monotonic_ms(), next = UINT64_MAX;
    for (unsigned i = 0; i < VSOCK_CLIENTS; ++i) {
        struct vsock_client *c = &vm->clients[i];
        if (c->fd < 0 || !c->deadline_ms)
            continue;
        if (c->deadline_ms <= now)
            client_close(vm, c, true);
        else if (c->deadline_ms < next)
            next = c->deadline_ms;
    }
    if (next == UINT64_MAX)
        return -1;
    uint64_t delay = next - now;
    return delay > INT_MAX ? INT_MAX : (int)delay;
}
