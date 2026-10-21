#include "vmm.h"

#include <inttypes.h>
#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <stdio.h>

uint64_t memory_snapshot(const struct memory *m, const char *stage)
{
    task_vm_info_data_t info = {0};
    mach_msg_type_number_t count = TASK_VM_INFO_COUNT;
    kern_return_t kr = task_info(mach_task_self(), TASK_VM_INFO,
                                (task_info_t)&info, &count);
    if (kr != KERN_SUCCESS || count < TASK_VM_INFO_REV1_COUNT) {
        vm_log("self TASK_VM_INFO failed: %d count=%u", kr, count);
        return UINT64_MAX;
    }
    vm_statistics64_data_t host = {0};
    mach_msg_type_number_t host_count = HOST_VM_INFO64_COUNT;
    mach_port_t host_port = mach_host_self();
    kern_return_t host_kr = host_statistics64(host_port, HOST_VM_INFO64,
        (host_info64_t)&host, &host_count);
    mach_port_deallocate(mach_task_self(), host_port);
    flockfile(stderr);
    fprintf(stderr, "sapphire-memory: {\"stage\":\"%s\","
            "\"footprint\":%" PRIu64 ",\"rss\":%" PRIu64 ","
            "\"internal\":%" PRIu64 ",\"external\":%" PRIu64 ","
            "\"compressed\":%" PRIu64 ",\"device\":%" PRIu64 ","
            "\"reusable\":%" PRIu64 ",\"virtual\":%" PRIu64 ","
            "\"region_count\":%d,\"host_page_size\":%d,\"ram_size\":%zu,"
            "\"host_stats_ok\":%s,\"host_free_pages\":%u,"
            "\"host_internal_pages\":%" PRIu64 ",\"host_wired_pages\":%u,"
            "\"host_compressor_pages\":%" PRIu64 ","
            "\"ram_regions\":[",
            stage, (uint64_t)info.phys_footprint, (uint64_t)info.resident_size,
            (uint64_t)info.internal, (uint64_t)info.external,
            (uint64_t)info.compressed, (uint64_t)info.device,
            (uint64_t)info.reusable, (uint64_t)info.virtual_size,
            info.region_count, info.page_size, m->size,
            host_kr == KERN_SUCCESS ? "true" : "false", host.free_count,
            (uint64_t)host.internal_page_count, host.wire_count,
            (uint64_t)host.compressor_page_count);
    mach_vm_address_t cursor = (uintptr_t)m->ram;
    mach_vm_address_t end = cursor + m->size;
    uint64_t mapped = 0, resident = 0, swapped = 0;
    unsigned regions = 0, queries = 0;
    while (m->ram && cursor < end && queries++ < 262144) {
        mach_vm_address_t address = cursor;
        mach_vm_size_t size = 0;
        natural_t depth = 0;
        vm_region_submap_info_data_64_t region;
        do {
            count = VM_REGION_SUBMAP_INFO_COUNT_64;
            kr = mach_vm_region_recurse(mach_task_self(), &address, &size,
                &depth, (vm_region_recurse_info_t)&region, &count);
            if (kr != KERN_SUCCESS || !region.is_submap)
                break;
            depth++;
        } while (depth < 16);
        if (kr != KERN_SUCCESS || region.is_submap || address >= end ||
            !size || address + size <= cursor)
            break;
        mapped += (address + size < end ? address + size : end) -
                  (address > cursor ? address : cursor);
        resident += (uint64_t)region.pages_resident * info.page_size;
        swapped += (uint64_t)region.pages_swapped_out * info.page_size;
        if (regions < 16) {
            fprintf(stderr, "%s{\"address\":%" PRIu64 ",\"size\":%" PRIu64
                ",\"object\":%u,\"offset\":%" PRIu64
                ",\"resident_pages\":%u,\"swapped_pages\":%u,"
                "\"references\":%u,\"shadow_depth\":%u,\"share_mode\":%u}",
                regions ? "," : "", (uint64_t)address, (uint64_t)size,
                region.object_id, (uint64_t)region.offset,
                region.pages_resident, region.pages_swapped_out,
                region.ref_count, region.shadow_depth, region.share_mode);
        }
        regions++;
        cursor = address + size;
    }
    fprintf(stderr, "],\"ram_region_count\":%u,\"ram_mapped_bytes\":%" PRIu64
            ",\"ram_region_resident_bytes\":%" PRIu64
            ",\"ram_region_swapped_bytes\":%" PRIu64
            ",\"ram_walk_complete\":%s}\n",
            regions, mapped, resident, swapped,
            !m->ram || cursor >= end ? "true" : "false");
    funlockfile(stderr);
    return info.phys_footprint;
}
