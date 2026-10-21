#include "vmm.h"

#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

uint16_t ld16(const void *p) { uint16_t v; memcpy(&v, p, 2); return v; }
uint32_t ld32(const void *p) { uint32_t v; memcpy(&v, p, 4); return v; }
uint64_t ld64(const void *p) { uint64_t v; memcpy(&v, p, 8); return v; }
void st16(void *p, uint16_t v) { memcpy(p, &v, 2); }
void st32(void *p, uint32_t v) { memcpy(p, &v, 4); }
void st64(void *p, uint64_t v) { memcpy(p, &v, 8); }

void *guest_ptr(const struct memory *m, uint64_t gpa, size_t size)
{
    if (gpa < RAM_BASE || gpa - RAM_BASE > m->size ||
        size > m->size - (size_t)(gpa - RAM_BASE))
        return NULL;
    return m->ram + (size_t)(gpa - RAM_BASE);
}

int memory_create(struct memory *m, size_t size)
{
    m->ram = mmap(NULL, size, PROT_READ | PROT_WRITE,
                  MAP_PRIVATE | MAP_ANON, -1, 0);
    if (m->ram == MAP_FAILED) {
        m->ram = NULL;
        return -1;
    }
    m->size = size;
    return 0;
}

void memory_destroy(struct memory *m)
{
    if (m->ram)
        munmap(m->ram, m->size);
    m->ram = NULL;
    m->size = 0;
}

int memory_report_free(struct vm *vm, uint64_t gpa, size_t size)
{
    size_t page = (size_t)sysconf(_SC_PAGESIZE);
    if (!page || (page & (page - 1)) || !guest_ptr(&vm->mem, gpa, size))
        return -1;
    vm->reported_free_bytes += size;
    uint64_t begin = (gpa + page - 1) & ~(uint64_t)(page - 1);
    uint64_t end = (gpa + size) & ~(uint64_t)(page - 1);
    if (end <= begin) {
        vm->report_skipped_bytes += size;
        return 0;
    }
    size_t aligned = (size_t)(end - begin);
    vm->report_skipped_bytes += size - aligned;
    if (!vm->reclaim || vm->reclaim(vm, begin, aligned))
        return -1;
    vm->reclaimed_bytes += aligned;
    return 0;
}

static int open_image(const char *path, size_t *size)
{
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    struct stat st;
    if (fd < 0)
        return -1;
    if (fstat(fd, &st) || !S_ISREG(st.st_mode) || st.st_size <= 0 ||
        (uint64_t)st.st_size > SIZE_MAX) {
        close(fd);
        errno = EINVAL;
        return -1;
    }
    *size = (size_t)st.st_size;
    return fd;
}

static int read_image(int fd, void *buf, size_t size)
{
    size_t done = 0;
    while (done < size) {
        ssize_t n = pread(fd, (uint8_t *)buf + done, size - done, (off_t)done);
        if (n < 0 && errno == EINTR)
            continue;
        if (n <= 0)
            return -1;
        done += (size_t)n;
    }
    return 0;
}

int boot_load(struct vm *vm)
{
    uint8_t header[64];
    size_t kernel_size, initrd_size;
    int kfd = open_image(vm->opt.kernel, &kernel_size);
    int ifd = -1, rc = -1;
    if (kfd < 0) {
        vm_log("open kernel: %s", strerror(errno));
        return -1;
    }
    if (kernel_size < sizeof(header) ||
        pread(kfd, header, sizeof(header), 0) != sizeof(header) ||
        ld32(header + 56) != 0x644d5241u || (ld64(header + 24) & 1u)) {
        vm_log("kernel must be a little-endian raw ARM64 Linux Image");
        goto out;
    }
    uint64_t offset = ld64(header + 8);
    uint64_t footprint = ld64(header + 16);
    if (!footprint) {
        offset = 0x80000;
        footprint = 32u * 1024u * 1024u;
    }
    if (footprint < kernel_size)
        footprint = kernel_size;
    if ((offset & 3) || offset > vm->mem.size ||
        footprint > vm->mem.size - offset) {
        vm_log("kernel Image does not fit guest RAM");
        goto out;
    }
    vm->boot.entry = RAM_BASE + offset;
    vm->boot.kernel_end = vm->boot.entry + footprint;
    vm->boot.dtb = (RAM_BASE + vm->mem.size - 2u * 1024u * 1024u) &
                   ~UINT64_C(0x1fffff);
    ifd = open_image(vm->opt.initrd, &initrd_size);
    if (ifd < 0) {
        vm_log("open initrd: %s", strerror(errno));
        goto out;
    }
    if (initrd_size > vm->boot.dtb - RAM_BASE)
        goto no_space;
    vm->boot.initrd_start = (vm->boot.dtb - initrd_size) & ~UINT64_C(0x1fffff);
    vm->boot.initrd_end = vm->boot.initrd_start + initrd_size;
    if (vm->boot.initrd_start < vm->boot.kernel_end)
        goto no_space;
    if (read_image(kfd, guest_ptr(&vm->mem, vm->boot.entry, kernel_size),
                   kernel_size) ||
        read_image(ifd, guest_ptr(&vm->mem, vm->boot.initrd_start, initrd_size),
                   initrd_size)) {
        vm_log("read boot image: %s", strerror(errno));
        goto out;
    }
    if (fdt_build(vm, guest_ptr(&vm->mem, vm->boot.dtb, 65536),
                  65536, &vm->boot.dtb_size))
        goto out;
    __builtin___clear_cache((char *)vm->mem.ram,
                           (char *)vm->mem.ram + offset + kernel_size);
    rc = 0;
    goto out;
no_space:
    vm_log("RAM is too small for the kernel, initrd and device tree");
out:
    close(kfd);
    if (ifd >= 0)
        close(ifd);
    return rc;
}
