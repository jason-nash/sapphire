#include "vmm.h"

#include <errno.h>
#include <fcntl.h>
#include <getopt.h>
#include <inttypes.h>
#include <limits.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

void vm_log(const char *format, ...)
{
    va_list args;
    va_start(args, format);
    flockfile(stderr);
    fputs("sapphire-vmm: ", stderr);
    vfprintf(stderr, format, args);
    fputc('\n', stderr);
    funlockfile(stderr);
    va_end(args);
}

static void usage(FILE *out)
{
    fputs("Usage: sapphire-vmm --kernel Image --initrd initrd.gz\n"
          "  --root-disk rootfs.squashfs --data-disk data.ext4\n"
          "  --memory-mib N --cpus N --net-fd FD --vsock PATH --console PATH\n"
          "  [--parent-fd FD]\n"
          "  [--initialize-data-disk]\n"
          "  [--memory-diagnostics]\n"
          "  sapphire-vmm --probe [--cpus N]\n"
          "\nARM64 macOS 15+; raw Linux Image; initrd handles root/data mounts.\n"
          "net-fd is a connected inherited Unix SOCK_DGRAM Ethernet endpoint.\n"
          "vsock accepts CONNECT <port>\\n and returns OK\\n after guest acceptance.\n"
          "parent-fd is an optional inherited pipe; EOF stops the VM.\n"
          "initialize-data-disk marks a host-created new disk for guest initialization.\n"
          "memory-diagnostics emits self memory JSON to stderr; SIGUSR1 requests a snapshot.\n"
          "SIGINT/SIGTERM/SIGHUP stop the VM; guest SYSTEM_RESET reboots it.\n",
          out);
}

static bool number(const char *s, uint64_t min, uint64_t max, uint64_t *value)
{
    char *end;
    if (!s[0] || s[0] < '0' || s[0] > '9')
        return false;
    errno = 0;
    unsigned long long n = strtoull(s, &end, 10);
    if (errno || *end || n < min || n > max)
        return false;
    *value = n;
    return true;
}

static int parse(int argc, char **argv, struct options *opt)
{
    enum { KERNEL = 256, INITRD, ROOT, DATA, MEMORY, CPUS, NET, VSOCK, CONSOLE,
           PROBE, PARENT, INITIALIZE_DATA, MEMORY_DIAGNOSTICS };
    static const struct option flags[] = {
        {"kernel", required_argument, NULL, KERNEL},
        {"initrd", required_argument, NULL, INITRD},
        {"root-disk", required_argument, NULL, ROOT},
        {"data-disk", required_argument, NULL, DATA},
        {"initialize-data-disk", no_argument, NULL, INITIALIZE_DATA},
        {"memory-diagnostics", no_argument, NULL, MEMORY_DIAGNOSTICS},
        {"memory-mib", required_argument, NULL, MEMORY},
        {"cpus", required_argument, NULL, CPUS},
        {"net-fd", required_argument, NULL, NET},
        {"parent-fd", required_argument, NULL, PARENT},
        {"vsock", required_argument, NULL, VSOCK},
        {"console", required_argument, NULL, CONSOLE},
        {"probe", no_argument, NULL, PROBE},
        {"help", no_argument, NULL, 'h'}, {NULL, 0, NULL, 0}
    };
    *opt = (struct options){.memory_mib = 1024, .cpus = 2, .net_fd = -1, .parent_fd = -1};
    int flag;
    uint64_t n;
    while ((flag = getopt_long(argc, argv, "h", flags, NULL)) != -1) {
        switch (flag) {
        case KERNEL: opt->kernel = optarg; break;
        case INITRD: opt->initrd = optarg; break;
        case ROOT: opt->root = optarg; break;
        case DATA: opt->data = optarg; break;
        case INITIALIZE_DATA: opt->initialize_data_disk = true; break;
        case MEMORY_DIAGNOSTICS: opt->memory_diagnostics = true; break;
        case VSOCK: opt->vsock = optarg; break;
        case CONSOLE: opt->console = optarg; break;
        case MEMORY:
            if (!number(optarg, 128, 1024u * 1024u, &n)) return -1;
            opt->memory_mib = (size_t)n;
            break;
        case CPUS:
            if (!number(optarg, 1, MAX_CPUS, &n)) return -1;
            opt->cpus = (unsigned)n;
            break;
        case NET:
            if (!number(optarg, 0, INT_MAX, &n)) return -1;
            opt->net_fd = (int)n;
            break;
        case PARENT:
            if (!number(optarg, 0, INT_MAX, &n)) return -1;
            opt->parent_fd = (int)n;
            break;
        case PROBE: opt->probe = true; break;
        case 'h': usage(stdout); exit(0);
        default: return -1;
        }
    }
    if (optind != argc)
        return -1;
    if (opt->probe)
        return 0;
    return opt->kernel && opt->initrd && opt->root && opt->data &&
        opt->vsock && opt->console && opt->net_fd >= 0 ? 0 : -1;
}

static int own_inherited_fds(struct options *opt)
{
    int net = fcntl(opt->net_fd, F_DUPFD_CLOEXEC, 3);
    if (net < 0) {
        vm_log("duplicate inherited network fd %d: %s", opt->net_fd, strerror(errno));
        return -1;
    }
    int parent = -1;
    if (opt->parent_fd >= 0) {
        parent = fcntl(opt->parent_fd, F_DUPFD_CLOEXEC, 3);
        if (parent < 0) {
            vm_log("duplicate inherited parent fd %d: %s", opt->parent_fd, strerror(errno));
            close(net);
            return -1;
        }
    }
    /* These handles belong to the process, not to any individual VM lifetime. */
    if (opt->net_fd >= 3)
        close(opt->net_fd);
    if (opt->parent_fd >= 3 && opt->parent_fd != opt->net_fd)
        close(opt->parent_fd);
    opt->net_fd = net;
    opt->parent_fd = parent;
    return 0;
}

static int run(const struct options *opt, uint64_t *uptime_ms)
{
    struct vm *vm = calloc(1, sizeof(*vm));
    if (!vm)
        return 3;
    vm->opt = *opt;
    vm->kq = vm->listener = vm->console_fd = -1;
    for (unsigned i = 0; i < DEVICE_COUNT; ++i)
        vm->dev[i].disk_fd = -1;
    vsock_init(vm);
    pthread_mutex_init(&vm->io_lock, NULL);
    atomic_init(&vm->stop, 0);
    int result = 3;
    if (opt->memory_diagnostics)
        memory_snapshot(&vm->mem, "before-memory-create");
    if (memory_create(&vm->mem, opt->memory_mib * 1024u * 1024u)) {
        vm_log("mmap guest RAM: %s", strerror(errno));
        goto out;
    }
    if (opt->memory_diagnostics)
        memory_snapshot(&vm->mem, "after-memory-create");
    if (devices_init(vm) || io_init(vm) || hv_start(vm))
        goto out;
    vm_log("running %u vCPUs, %zu MiB demand-backed RAM; balloon free-page reporting enabled",
           opt->cpus, opt->memory_mib);
    uint64_t began = monotonic_ms();
    result = io_run(vm);
    *uptime_ms = monotonic_ms() - began;
out:
    if (opt->memory_diagnostics)
        memory_snapshot(&vm->mem, "before-hv-stop");
    hv_stop(vm);
    devices_destroy(vm);
    io_destroy(vm);
    vm_log("stopped reason=%d net_rx=%" PRIu64 " net_tx=%" PRIu64
           " net_drop=%" PRIu64 " vsock_rx_bytes=%" PRIu64 " vsock_tx_bytes=%" PRIu64,
           result, vm->net_rx, vm->net_tx, vm->net_drop, vm->vsock_rx, vm->vsock_tx);
    vm_log("memory reported_free_bytes=%" PRIu64 " reclaimed_mapping_bytes=%" PRIu64
           " skipped_partial_host_page_bytes=%" PRIu64,
           vm->reported_free_bytes, vm->reclaimed_bytes, vm->report_skipped_bytes);
    memory_destroy(&vm->mem);
    if (opt->memory_diagnostics)
        memory_snapshot(&vm->mem, "after-memory-destroy");
    pthread_mutex_destroy(&vm->io_lock);
    free(vm);
    return result;
}

int main(int argc, char **argv)
{
    struct options opt;
    if (parse(argc, argv, &opt)) {
        usage(stderr);
        return 64;
    }
    if (opt.probe)
        return hv_probe(opt.cpus);
    if (own_inherited_fds(&opt))
        return 1;
    signal(SIGPIPE, SIG_IGN);
    signal(SIGINT, SIG_IGN);
    signal(SIGTERM, SIG_IGN);
    signal(SIGHUP, SIG_IGN);
    if (opt.memory_diagnostics)
        signal(SIGUSR1, SIG_IGN);
    int result;
    struct reset_policy policy = {0};
    do {
        uint64_t uptime_ms = 0;
        result = run(&opt, &uptime_ms);
        if (result == 2) {
            if (!reset_allowed(&policy, uptime_ms)) {
                vm_log("rapid reset limit reached: %u consecutive resets with "
                       "uptime below %" PRIu64 " ms; refusing reboot; inspect %s",
                       policy.rapid_resets, RESET_STABLE_MS, opt.console);
                result = 3;
                break;
            }
            vm_log("guest requested reset after %" PRIu64
                   " ms; consecutive rapid resets=%u/%u",
                   uptime_ms, policy.rapid_resets, RAPID_RESET_LIMIT);
        }
    } while (result == 2);
    close(opt.net_fd);
    if (opt.parent_fd >= 0)
        close(opt.parent_fd);
    return result == 1 ? 0 : 1;
}
