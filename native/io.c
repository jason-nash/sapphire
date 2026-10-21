#include "vmm.h"

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>
#include <sys/event.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

int fd_nonblock(int fd)
{
    int flags = fcntl(fd, F_GETFL);
    if (flags < 0 || fcntl(fd, F_SETFL, flags | O_NONBLOCK) ||
        fcntl(fd, F_SETFD, FD_CLOEXEC))
        return -1;
    return 0;
}

void io_wake(struct vm *vm)
{
    if (vm->kq < 0)
        return;
    struct kevent ev;
    EV_SET(&ev, 1, EVFILT_USER, 0, NOTE_TRIGGER, 0, NULL);
    (void)kevent(vm->kq, &ev, 1, NULL, 0, NULL);
}

static int watch(struct vm *vm, uintptr_t id, int16_t filter)
{
    struct kevent ev;
    EV_SET(&ev, id, filter, EV_ADD | EV_ENABLE, 0, 0, NULL);
    return kevent(vm->kq, &ev, 1, NULL, 0, NULL);
}

int io_init(struct vm *vm)
{
    struct sockaddr_un addr = {.sun_family = AF_UNIX};
    struct stat st;
    int type = 0;
    socklen_t type_size = sizeof(type);
    struct sockaddr_storage peer;
    socklen_t peer_size = sizeof(peer);
    if (getsockopt(vm->opt.net_fd, SOL_SOCKET, SO_TYPE, &type, &type_size)) {
        vm_log("network fd %d getsockopt: %s", vm->opt.net_fd, strerror(errno));
        return -1;
    }
    if (type != SOCK_DGRAM) {
        vm_log("network fd %d is not SOCK_DGRAM", vm->opt.net_fd);
        return -1;
    }
    if (getpeername(vm->opt.net_fd, (struct sockaddr *)&peer, &peer_size)) {
        vm_log("network fd %d getpeername: %s", vm->opt.net_fd, strerror(errno));
        return -1;
    }
    if (peer.ss_family != AF_UNIX || fd_nonblock(vm->opt.net_fd)) {
        vm_log("network fd %d must be a usable connected Unix datagram endpoint",
               vm->opt.net_fd);
        return -1;
    }
    if (vm->opt.parent_fd >= 0 &&
        (vm->opt.parent_fd == vm->opt.net_fd ||
         fstat(vm->opt.parent_fd, &st) || !S_ISFIFO(st.st_mode) ||
         fd_nonblock(vm->opt.parent_fd))) {
        vm_log("--parent-fd must be a distinct inherited lifetime pipe");
        return -1;
    }
    if (strlen(vm->opt.vsock) >= sizeof(addr.sun_path)) {
        vm_log("--vsock path exceeds UNIX socket path limit");
        return -1;
    }
    memcpy(addr.sun_path, vm->opt.vsock, strlen(vm->opt.vsock) + 1);
    if (!lstat(vm->opt.vsock, &st) || errno != ENOENT) {
        vm_log("--vsock path already exists or cannot be inspected");
        return -1;
    }
    vm->console_fd = open(vm->opt.console, O_WRONLY | O_CREAT | O_APPEND |
                          O_CLOEXEC | O_NOFOLLOW, 0600);
    if (vm->console_fd < 0) {
        vm_log("open console: %s", strerror(errno));
        return -1;
    }
    vm->listener = socket(AF_UNIX, SOCK_STREAM, 0);
    if (vm->listener < 0 || fd_nonblock(vm->listener) ||
        bind(vm->listener, (struct sockaddr *)&addr, sizeof(addr))) {
        vm_log("bind vsock bridge: %s", strerror(errno));
        return -1;
    }
    vm->socket_owned = true;
    if (lstat(vm->opt.vsock, &st))
        return -1;
    vm->socket_dev = st.st_dev;
    vm->socket_ino = st.st_ino;
    if (chmod(vm->opt.vsock, 0600) || listen(vm->listener, VSOCK_CLIENTS))
        return -1;
    vm->kq = kqueue();
    if (vm->kq < 0 || fcntl(vm->kq, F_SETFD, FD_CLOEXEC))
        return -1;
    struct kevent ev;
    EV_SET(&ev, 1, EVFILT_USER, EV_ADD | EV_CLEAR, 0, 0, NULL);
    if (kevent(vm->kq, &ev, 1, NULL, 0, NULL) ||
        watch(vm, vm->opt.net_fd, EVFILT_READ) ||
        watch(vm, vm->listener, EVFILT_READ) ||
        watch(vm, SIGINT, EVFILT_SIGNAL) ||
        watch(vm, SIGTERM, EVFILT_SIGNAL) ||
        watch(vm, SIGHUP, EVFILT_SIGNAL))
        return -1;
    if (vm->opt.parent_fd >= 0 && watch(vm, vm->opt.parent_fd, EVFILT_READ))
        return -1;
    if (vm->opt.memory_diagnostics && watch(vm, SIGUSR1, EVFILT_SIGNAL))
        return -1;
    vm->wake = io_wake;
    return 0;
}

int io_run(struct vm *vm)
{
    struct kevent events[64];
    while (!atomic_load(&vm->stop)) {
        pthread_mutex_lock(&vm->io_lock);
        vsock_drain(vm);
        int timeout = vsock_timeout(vm);
        int clock_timeout = rtc_timeout(vm);
        if (clock_timeout >= 0 && (timeout < 0 || clock_timeout < timeout))
            timeout = clock_timeout;
        vsock_update_events(vm);
        pthread_mutex_unlock(&vm->io_lock);
        if (atomic_load(&vm->stop))
            break;
        struct timespec ts = {.tv_sec = timeout / 1000,
                               .tv_nsec = (timeout % 1000) * 1000000L};
        int n = kevent(vm->kq, NULL, 0, events, 64, timeout < 0 ? NULL : &ts);
        if (n < 0 && errno == EINTR)
            continue;
        if (n < 0) {
            vm_log("kevent: %s", strerror(errno));
            vm_request_stop(vm, 3);
            break;
        }
        pthread_mutex_lock(&vm->io_lock);
        /* Accept after client events so descriptor reuse cannot match stale events. */
        bool accept_ready = false;
        for (int i = 0; i < n; ++i) {
            const struct kevent *e = &events[i];
            if (e->filter == EVFILT_SIGNAL) {
                if (vm->opt.memory_diagnostics && e->ident == SIGUSR1)
                    memory_snapshot(&vm->mem, "requested");
                else
                    vm_request_stop(vm, 1);
            } else if (e->filter == EVFILT_USER) {
                continue;
            } else if ((int)e->ident == vm->opt.parent_fd) {
                char bytes[64];
                ssize_t count = read(vm->opt.parent_fd, bytes, sizeof(bytes));
                if ((e->flags & (EV_EOF | EV_ERROR)) ||
                    !count || (count < 0 && errno != EAGAIN &&
                               errno != EWOULDBLOCK && errno != EINTR)) {
                    vm_log("parent lifetime pipe closed");
                    vm_request_stop(vm, 1);
                }
            } else if ((int)e->ident == vm->opt.net_fd) {
                if (e->flags & (EV_EOF | EV_ERROR)) {
                    vm_log("network endpoint closed");
                    vm_request_stop(vm, 1);
                } else {
                    net_receive(vm);
                }
            } else if ((int)e->ident == vm->listener) {
                if (e->flags & (EV_EOF | EV_ERROR))
                    vm_request_stop(vm, 3);
                else
                    accept_ready = true;
            } else {
                vsock_event(vm, (int)e->ident, e->filter == EVFILT_READ,
                             e->filter == EVFILT_WRITE, e->flags);
            }
        }
        if (accept_ready)
            vsock_accept(vm);
        pthread_mutex_unlock(&vm->io_lock);
    }
    return atomic_load(&vm->stop);
}

void io_destroy(struct vm *vm)
{
    vm->wake = NULL;
    if (vm->listener >= 0)
        close(vm->listener);
    if (vm->kq >= 0)
        close(vm->kq);
    vm->listener = vm->kq = -1;
    if (vm->console_fd >= 0) {
        console_flush(vm);
        close(vm->console_fd);
    }
    vm->console_fd = -1;
    if (vm->socket_owned) {
        struct stat st;
        if (!lstat(vm->opt.vsock, &st) && st.st_dev == vm->socket_dev &&
            st.st_ino == vm->socket_ino && S_ISSOCK(st.st_mode))
            unlink(vm->opt.vsock);
        vm->socket_owned = false;
    }
}
