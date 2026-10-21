#include <errno.h>
#include <libproc.h>
#include <limits.h>
#include <mach/mach_time.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <unistd.h>

static void json_string(const char *value) {
    putchar('"');
    for (const unsigned char *p = (const unsigned char *)value; *p; p++) {
        if (*p == '"' || *p == '\\') printf("\\%c", *p);
        else if (*p < 32) printf("\\u%04x", *p);
        else putchar(*p);
    }
    putchar('"');
}

int main(int argc, char **argv) {
    if (argc < 2) {
        fprintf(stderr, "usage: footprint ROOT_PID [ROOT_PID ...]\n");
        return 2;
    }
    mach_timebase_info_data_t timebase;
    if (mach_timebase_info(&timebase) != KERN_SUCCESS || !timebase.denom) return 1;
    int capacity = proc_listallpids(NULL, 0);
    if (capacity <= 0 || capacity > 1000000) return 1;
    capacity += 1024;
    pid_t *pids = calloc((size_t)capacity, sizeof(*pids));
    pid_t *parents = calloc((size_t)capacity, sizeof(*parents));
    bool *selected = calloc((size_t)capacity, sizeof(*selected));
    if (!pids || !parents || !selected) return 1;
    int count = proc_listallpids(pids, capacity * (int)sizeof(*pids));
    if (count <= 0 || count >= capacity) {
        fprintf(stderr, "process enumeration was incomplete\n");
        return 1;
    }
    for (int i = 0; i < count; i++) {
        struct proc_bsdinfo info = {0};
        if (proc_pidinfo(pids[i], PROC_PIDTBSDINFO, 0, &info, sizeof(info)) == sizeof(info))
            parents[i] = (pid_t)info.pbi_ppid;
    }
    for (int a = 1; a < argc; a++) {
        char *end = NULL;
        long pid = strtol(argv[a], &end, 10);
        if (!end || *end || pid <= 1 || pid > INT_MAX) return 2;
        bool found = false;
        for (int i = 0; i < count; i++) {
            if (pids[i] == (pid_t)pid) selected[i] = found = true;
        }
        if (!found) {
            fprintf(stderr, "root process %ld is no longer running\n", pid);
            return 1;
        }
    }
    bool changed;
    do {
        changed = false;
        for (int i = 0; i < count; i++) {
            if (selected[i]) continue;
            for (int j = 0; j < count; j++) {
                if (selected[j] && parents[i] == pids[j]) {
                    selected[i] = changed = true;
                    break;
                }
            }
        }
    } while (changed);
    printf("[");
    bool first = true;
    for (int i = 0; i < count; i++) {
        if (!selected[i]) continue;
        struct rusage_info_v4 usage = {0};
        char path[PROC_PIDPATHINFO_MAXSIZE] = {0};
        if (proc_pid_rusage(pids[i], RUSAGE_INFO_V4, (rusage_info_t *)&usage) != 0 ||
            proc_pidpath(pids[i], path, sizeof(path)) <= 0) {
            fprintf(stderr, "cannot measure process %d: %s\n", pids[i], strerror(errno));
            return 1;
        }
        printf("%s{\"pid\":%d,\"parent\":%d,\"start\":%llu,\"path\":",
               first ? "" : ",", pids[i], parents[i], usage.ri_proc_start_abstime);
        json_string(path);
        /* proc_pid_rusage CPU times use Mach absolute units on this API. */
        printf(",\"footprint\":%llu,\"rss\":%llu,\"cpuNs\":%llu,"
               "\"idleWakeups\":%llu,\"interruptWakeups\":%llu,"
               "\"diskReadBytes\":%llu,\"diskWriteBytes\":%llu}",
               usage.ri_phys_footprint, usage.ri_resident_size,
               (unsigned long long)(((__uint128_t)usage.ri_user_time + usage.ri_system_time) *
                                    timebase.numer / timebase.denom),
               usage.ri_pkg_idle_wkups, usage.ri_interrupt_wkups,
               usage.ri_diskio_bytesread, usage.ri_diskio_byteswritten);
        first = false;
    }
    puts("]");
    free(selected);
    free(parents);
    free(pids);
    return 0;
}
