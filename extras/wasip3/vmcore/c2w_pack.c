/*
 * Files embedded into the emulator module.
 *
 * WASI 0.3 builds do not use wasi-vfs. Instead the files of the pack
 * directory (BIOS images, bochsrc, boot.iso, rootfs.bin) are embedded with
 * `.incbin` and served to the emulator through wrappers of the POSIX file
 * functions it uses (`open`, `read`, ... installed with wasm-ld's --wrap).
 * Paths under /pack/ resolve to the embedded data; everything else is
 * forwarded to wasi-libc.
 *
 * The list of files is provided by the generated header c2w_pack_list.h:
 *   C2W_PACK_FILE(identifier, "name", "/absolute/path/at/build/time")
 * and the bridge description by the C2W_BRIDGE_DESCRIPTION_PATH macro.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/uio.h>
#include <unistd.h>

#include "c2w_pack_list.h"

#define STR_(x) #x
#define STR(x) STR_(x)

/* Embed every file as a read-only blob. */
#define C2W_PACK_FILE(ident, name, path)                                     \
  __asm__(".section .rodata.c2w_pack_" #ident ",\"\",@\n"                   \
          ".balign 16\n"                                                     \
          ".global c2w_pack_" #ident "_start\n"                              \
          ".type c2w_pack_" #ident "_start,@object\n"                        \
          "c2w_pack_" #ident "_start:\n"                                     \
          ".incbin \"" path "\"\n"                                           \
          ".global c2w_pack_" #ident "_end\n"                                \
          ".type c2w_pack_" #ident "_end,@object\n"                          \
          "c2w_pack_" #ident "_end:\n"                                       \
          ".byte 0\n"                                                        \
          ".size c2w_pack_" #ident "_start, c2w_pack_" #ident "_end - c2w_pack_" #ident "_start\n" \
          ".size c2w_pack_" #ident "_end, 1\n");                             \
  extern const uint8_t c2w_pack_##ident##_start[];                           \
  extern const uint8_t c2w_pack_##ident##_end[];
C2W_PACK_FILES
#undef C2W_PACK_FILE

__asm__(".section .rodata.c2w_bridge_description,\"\",@\n"
        ".balign 16\n"
        ".global c2w_bridge_description_start\n"
        ".type c2w_bridge_description_start,@object\n"
        "c2w_bridge_description_start:\n"
        ".incbin \"" C2W_BRIDGE_DESCRIPTION_PATH "\"\n"
        ".global c2w_bridge_description_end\n"
        ".type c2w_bridge_description_end,@object\n"
        "c2w_bridge_description_end:\n"
        ".byte 0\n"
        ".size c2w_bridge_description_start, c2w_bridge_description_end - c2w_bridge_description_start\n"
        ".size c2w_bridge_description_end, 1\n");

typedef struct {
  const char *name;
  const uint8_t *start;
  const uint8_t *end;
} pack_file_t;

static const pack_file_t pack_files[] = {
#define C2W_PACK_FILE(ident, name, path) {name, c2w_pack_##ident##_start, c2w_pack_##ident##_end},
    C2W_PACK_FILES
#undef C2W_PACK_FILE
};
#define NUM_PACK_FILES (sizeof(pack_files) / sizeof(pack_files[0]))

#define PACK_PREFIX "/pack/"
#define PACK_FD_BASE 0x40000
#define PACK_MAX_OPEN 64

typedef struct {
  int file; /* index into pack_files or -1 when free */
  off_t pos;
} pack_fd_t;

static pack_fd_t pack_fds[PACK_MAX_OPEN];

static int pack_lookup(const char *path) {
  if (strncmp(path, PACK_PREFIX, strlen(PACK_PREFIX)) != 0)
    return -1;
  const char *name = path + strlen(PACK_PREFIX);
  for (size_t i = 0; i < NUM_PACK_FILES; i++) {
    if (strcmp(pack_files[i].name, name) == 0)
      return (int)i;
  }
  return -1;
}

static bool is_pack_path(const char *path) {
  return strncmp(path, PACK_PREFIX, strlen(PACK_PREFIX)) == 0;
}

static pack_fd_t *pack_fd(int fd) {
  if (fd < PACK_FD_BASE || fd >= PACK_FD_BASE + PACK_MAX_OPEN)
    return NULL;
  pack_fd_t *p = &pack_fds[fd - PACK_FD_BASE];
  return p->file >= 0 ? p : NULL;
}

static size_t pack_size(const pack_file_t *f) { return (size_t)(f->end - f->start); }

static void pack_stat(const pack_file_t *f, struct stat *st) {
  memset(st, 0, sizeof(*st));
  st->st_mode = S_IFREG | 0444;
  st->st_size = (off_t)pack_size(f);
  st->st_nlink = 1;
  st->st_blksize = 4096;
  st->st_blocks = (st->st_size + 511) / 512;
}

/* ---- wrappers ---------------------------------------------------------- */

int __real_open(const char *path, int flags, ...);
int __real___wasilibc_open_nomode(const char *path, int flags);
ssize_t __real_read(int fd, void *buf, size_t n);
ssize_t __real_readv(int fd, const struct iovec *iov, int iovcnt);
ssize_t __real_pread(int fd, void *buf, size_t n, off_t off);
off_t __real_lseek(int fd, off_t off, int whence);
int __real_close(int fd);
int __real_fstat(int fd, struct stat *st);
int __real_stat(const char *path, struct stat *st);
int __real_access(const char *path, int mode);

static int pack_open(const char *path, int flags) {
  int idx = pack_lookup(path);
  if (idx < 0) {
    errno = ENOENT;
    return -1;
  }
  /* Writable opens are accepted so that emulated disks can be attached;
   * the data is read-only and write() fails on these descriptors. */
  (void)flags;
  static bool initialized;
  if (!initialized) {
    for (int i = 0; i < PACK_MAX_OPEN; i++)
      pack_fds[i].file = -1;
    initialized = true;
  }
  for (int i = 0; i < PACK_MAX_OPEN; i++) {
    if (pack_fds[i].file < 0) {
      pack_fds[i].file = idx;
      pack_fds[i].pos = 0;
      return PACK_FD_BASE + i;
    }
  }
  errno = EMFILE;
  return -1;
}

/* fopen() in wasi-libc opens through this function rather than open(). */
int __wrap___wasilibc_open_nomode(const char *path, int flags) {
  if (is_pack_path(path))
    return pack_open(path, flags);
  return __real___wasilibc_open_nomode(path, flags);
}

int __wrap_open(const char *path, int flags, ...) {
  if (is_pack_path(path))
    return pack_open(path, flags);
  mode_t mode = 0;
  if (flags & O_CREAT) {
    va_list ap;
    va_start(ap, flags);
    mode = va_arg(ap, mode_t);
    va_end(ap);
    return __real_open(path, flags, mode);
  }
  return __real_open(path, flags);
}

static ssize_t pack_pread(pack_fd_t *p, void *buf, size_t n, off_t off) {
  const pack_file_t *f = &pack_files[p->file];
  size_t size = pack_size(f);
  if (off < 0) {
    errno = EINVAL;
    return -1;
  }
  if ((size_t)off >= size)
    return 0;
  if (n > size - (size_t)off)
    n = size - (size_t)off;
  memcpy(buf, f->start + off, n);
  return (ssize_t)n;
}

ssize_t __wrap_read(int fd, void *buf, size_t n) {
  pack_fd_t *p = pack_fd(fd);
  if (p == NULL)
    return __real_read(fd, buf, n);
  ssize_t r = pack_pread(p, buf, n, p->pos);
  if (r > 0)
    p->pos += r;
  return r;
}

ssize_t __wrap_readv(int fd, const struct iovec *iov, int iovcnt) {
  pack_fd_t *p = pack_fd(fd);
  if (p == NULL)
    return __real_readv(fd, iov, iovcnt);
  ssize_t total = 0;
  for (int i = 0; i < iovcnt; i++) {
    ssize_t r = pack_pread(p, iov[i].iov_base, iov[i].iov_len, p->pos);
    if (r < 0)
      return total > 0 ? total : r;
    p->pos += r;
    total += r;
    if ((size_t)r < iov[i].iov_len)
      break;
  }
  return total;
}

ssize_t __wrap_pread(int fd, void *buf, size_t n, off_t off) {
  pack_fd_t *p = pack_fd(fd);
  if (p == NULL)
    return __real_pread(fd, buf, n, off);
  return pack_pread(p, buf, n, off);
}

off_t __wrap_lseek(int fd, off_t off, int whence) {
  pack_fd_t *p = pack_fd(fd);
  if (p == NULL)
    return __real_lseek(fd, off, whence);
  off_t base;
  switch (whence) {
  case SEEK_SET:
    base = 0;
    break;
  case SEEK_CUR:
    base = p->pos;
    break;
  case SEEK_END:
    base = (off_t)pack_size(&pack_files[p->file]);
    break;
  default:
    errno = EINVAL;
    return -1;
  }
  if (base + off < 0) {
    errno = EINVAL;
    return -1;
  }
  p->pos = base + off;
  return p->pos;
}

int __wrap_close(int fd) {
  pack_fd_t *p = pack_fd(fd);
  if (p == NULL)
    return __real_close(fd);
  p->file = -1;
  return 0;
}

int __wrap_fstat(int fd, struct stat *st) {
  pack_fd_t *p = pack_fd(fd);
  if (p == NULL)
    return __real_fstat(fd, st);
  pack_stat(&pack_files[p->file], st);
  return 0;
}

int __wrap_stat(const char *path, struct stat *st) {
  if (!is_pack_path(path))
    return __real_stat(path, st);
  int idx = pack_lookup(path);
  if (idx < 0) {
    errno = ENOENT;
    return -1;
  }
  pack_stat(&pack_files[idx], st);
  return 0;
}

int __wrap_access(const char *path, int mode) {
  if (!is_pack_path(path))
    return __real_access(path, mode);
  if (pack_lookup(path) < 0) {
    errno = ENOENT;
    return -1;
  }
  if (mode & W_OK) {
    errno = EROFS;
    return -1;
  }
  return 0;
}
