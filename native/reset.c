#include "vmm.h"

bool reset_allowed(struct reset_policy *policy, uint64_t uptime_ms)
{
    if (uptime_ms >= RESET_STABLE_MS) {
        policy->rapid_resets = 0;
        return true;
    }
    if (policy->rapid_resets < RAPID_RESET_LIMIT)
        ++policy->rapid_resets;
    return policy->rapid_resets < RAPID_RESET_LIMIT;
}
