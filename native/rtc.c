#include "vmm.h"

#include <limits.h>
#include <time.h>

static uint64_t wall_ms(void)
{
    struct timespec ts;
    if (clock_gettime(CLOCK_REALTIME, &ts))
        return 0;
    return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static uint32_t counter(const struct vm *vm, uint64_t now)
{
    return vm->rtc_control ?
        (uint32_t)((int64_t)(now / 1000) + vm->rtc_offset) : vm->rtc_load;
}

static void update_irq(struct vm *vm)
{
    if (vm->set_irq)
        vm->set_irq(vm, RTC_IRQ, (vm->rtc_raw & vm->rtc_imsc & 1) != 0);
}

static void schedule(struct vm *vm, uint64_t now)
{
    if (!vm->rtc_control || !vm->rtc_match_set) {
        vm->rtc_deadline_ms = 0;
        return;
    }
    uint32_t delta = vm->rtc_match - counter(vm, now);
    vm->rtc_deadline_ms = (now / 1000 + delta) * 1000;
}

int rtc_init(struct vm *vm)
{
    uint64_t now = wall_ms();
    if (!now)
        return -1;
    /* Linux enables the counter during probe; seed its initial value with UTC. */
    vm->rtc_load = (uint32_t)(now / 1000);
    return 0;
}

int rtc_timeout(struct vm *vm)
{
    if (!vm->rtc_deadline_ms)
        return -1;
    uint64_t now = wall_ms();
    if (now >= vm->rtc_deadline_ms) {
        vm->rtc_raw = 1;
        /* A 32-bit seconds counter reaches this match again after wrapping. */
        vm->rtc_deadline_ms += UINT64_C(0x100000000) * 1000;
        update_irq(vm);
    }
    uint64_t delta = vm->rtc_deadline_ms > now ? vm->rtc_deadline_ms - now : 0;
    return delta > INT_MAX ? INT_MAX : (int)delta;
}

uint32_t rtc_read(struct vm *vm, uint32_t off)
{
    static const uint8_t ids[8] = {0x31, 0x10, 0x04, 0x00, 0x0d, 0xf0, 0x05, 0xb1};
    if (off >= 0xfe0 && off <= 0xffc && !(off & 3))
        return ids[(off - 0xfe0) / 4];
    (void)rtc_timeout(vm);
    switch (off) {
    case 0x00: return counter(vm, wall_ms());
    case 0x04: return vm->rtc_match;
    case 0x08: return vm->rtc_load;
    case 0x0c: return vm->rtc_control;
    case 0x10: return vm->rtc_imsc;
    case 0x14: return vm->rtc_raw;
    case 0x18: return vm->rtc_raw & vm->rtc_imsc;
    default: return 0;
    }
}

void rtc_write(struct vm *vm, uint32_t off, uint32_t value)
{
    uint64_t now = wall_ms();
    (void)rtc_timeout(vm);
    switch (off) {
    case 0x04:
        vm->rtc_match = value;
        vm->rtc_match_set = true;
        schedule(vm, now);
        break;
    case 0x08:
        vm->rtc_load = value;
        vm->rtc_offset = (int64_t)value - (int64_t)(now / 1000);
        schedule(vm, now);
        break;
    case 0x0c:
        /* PL031 resets its count on a control write while already enabled. */
        if (vm->rtc_control)
            vm->rtc_load = 0;
        vm->rtc_control = value & 1;
        vm->rtc_offset = (int64_t)vm->rtc_load - (int64_t)(now / 1000);
        schedule(vm, now);
        break;
    case 0x10:
        vm->rtc_imsc = value & 1;
        break;
    case 0x1c:
        if (value & 1)
            vm->rtc_raw = 0;
        break;
    default: return;
    }
    (void)rtc_timeout(vm);
    update_irq(vm);
    if (vm->wake)
        vm->wake(vm);
}
