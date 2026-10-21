#include "vmm.h"

#include <string.h>

bool queue_valid(struct vm *vm, const struct virtq *q)
{
    return q->num && q->num <= QUEUE_MAX && !(q->num & (q->num - 1)) &&
        !(q->desc & 15) && !(q->avail & 1) && !(q->used & 3) &&
        guest_ptr(&vm->mem, q->desc, 16 * q->num) &&
        guest_ptr(&vm->mem, q->avail, 6 + 2 * q->num) &&
        guest_ptr(&vm->mem, q->used, 6 + 8 * q->num);
}

void device_fail(struct vm *vm, struct device *d, const char *why)
{
    if (!(d->status & 64))
        vm_log("virtio device %u: %s; needs reset", d->index, why);
    d->status |= 64; /* DEVICE_NEEDS_RESET */
    d->interrupt |= 2;
    if (vm->set_irq)
        vm->set_irq(vm, d->irq, true);
}

int queue_pop(struct vm *vm, struct device *d, struct virtq *q, struct chain *c)
{
    if (!q->ready || !(d->status & 4) || (d->status & (64 | 128)))
        return 0;
    if (!queue_valid(vm, q))
        goto bad;
    uint8_t *avail = guest_ptr(&vm->mem, q->avail, 6 + 2 * q->num);
    uint16_t idx = __atomic_load_n((uint16_t *)(void *)(avail + 2),
                                   __ATOMIC_ACQUIRE);
    uint16_t pending = (uint16_t)(idx - q->last_avail);
    if (!pending)
        return 0;
    if (pending > q->num)
        goto bad;
    uint16_t head = ld16(avail + 4 + 2 * (q->last_avail % q->num));
    if (head >= q->num)
        goto bad;
    memset(c, 0, sizeof(*c));
    c->head = head;
    uint16_t current = head;
    uint32_t limit = d->id == 5 ? 256u * 1024u * 1024u :
                     d->id == 2 ? BLOCK_CHAIN_MAX : MAX_TRANSFER;
    uint64_t seen[QUEUE_MAX / 64] = {0};
    bool saw_write = false;
    for (unsigned i = 0; i < q->num; ++i) {
        if (current >= q->num || (seen[current / 64] & (UINT64_C(1) << (current % 64))))
            goto bad;
        seen[current / 64] |= UINT64_C(1) << (current % 64);
        uint8_t raw[16];
        memcpy(raw, guest_ptr(&vm->mem, q->desc + 16u * current, 16), 16);
        uint64_t addr = ld64(raw);
        uint32_t len = ld32(raw + 8);
        uint16_t flags = ld16(raw + 12);
        bool write = flags & 2;
        if ((flags & ~3u) || (saw_write && !write) || len > limit ||
            c->readable + c->writable > limit - len)
            goto bad;
        void *ptr = guest_ptr(&vm->mem, addr, len);
        if (!ptr)
            goto bad;
        c->seg[c->count++] = (struct segment){ptr, addr, len, write};
        if (write)
            c->writable += len;
        else
            c->readable += len;
        saw_write |= write;
        if (!(flags & 1)) {
            q->last_avail++;
            return 1;
        }
        current = ld16(raw + 14);
    }
bad:
    device_fail(vm, d, "invalid split virtqueue or descriptor chain");
    return -1;
}

void queue_complete(struct vm *vm, struct device *d, struct virtq *q,
                    const struct chain *c, uint32_t len)
{
    uint8_t *used = guest_ptr(&vm->mem, q->used, 6 + 8 * q->num);
    if (!used || len > c->writable) {
        device_fail(vm, d, "invalid completion length or used ring");
        return;
    }
    uint8_t *elem = used + 4 + 8 * (q->next_used % q->num);
    st32(elem, c->head);
    st32(elem + 4, len);
    __atomic_store_n((uint16_t *)(void *)(used + 2), ++q->next_used,
                     __ATOMIC_RELEASE);
    /* Pair with the guest's notification-enable barrier and used.idx recheck.
     * A release store alone does not order this store before the flags load. */
    __atomic_thread_fence(__ATOMIC_SEQ_CST);
    uint8_t *avail = guest_ptr(&vm->mem, q->avail, 2);
    if (!avail || !(__atomic_load_n((uint16_t *)(void *)avail, __ATOMIC_RELAXED) & 1)) {
        bool already_pending = d->interrupt != 0;
        d->interrupt |= 1;
        if (!already_pending && vm->set_irq)
            vm->set_irq(vm, d->irq, true);
    }
}

static size_t chain_copy(const struct chain *c, size_t offset, void *buf,
                         size_t len, bool write)
{
    size_t done = 0;
    for (unsigned i = 0; i < c->count && done < len; ++i) {
        const struct segment *s = &c->seg[i];
        if (s->write != write)
            continue;
        if (offset >= s->len) {
            offset -= s->len;
            continue;
        }
        size_t n = s->len - offset;
        if (n > len - done)
            n = len - done;
        if (write)
            memcpy(s->data + offset, (uint8_t *)buf + done, n);
        else
            memcpy((uint8_t *)buf + done, s->data + offset, n);
        offset = 0;
        done += n;
    }
    return done;
}

size_t chain_read(const struct chain *c, size_t off, void *buf, size_t len)
{
    return chain_copy(c, off, buf, len, false);
}

size_t chain_write(const struct chain *c, size_t off, const void *buf, size_t len)
{
    return chain_copy(c, off, (void *)buf, len, true);
}
