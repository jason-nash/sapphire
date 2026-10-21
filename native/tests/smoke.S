/* A raw Image-shaped payload for testing our own HV VM, never a Linux substitute. */
.section .text
.global _start
_start:
    b primary
    .long 0
    .quad 0x80000
    .quad 0x10000
    .quad 0
    .quad 0
    .quad 0
    .quad 0
    .long 0x644d5241
    .long 0
primary:
    mov x20, #0x09000000
    ldr w1, [x0]
    mov w2, #0x0dd0
    movk w2, #0xedfe, lsl #16
    cmp w1, w2
    b.ne fail
    mov w1, #'B'
    str w1, [x20]
    mov w1, #10
    str w1, [x20]
    mov x0, #0x84000000
    hvc #0
    cmp x0, #0x10000
    b.ne fail
    mov x0, #3
    movk x0, #0xc400, lsl #16
    mov x1, #1
    adr x2, secondary
    mov x3, #0x1234
    hvc #0
    cbnz x0, fail
    adr x21, flag
1:
    ldar w1, [x21]
    cbz w1, 1b
    cmp w1, #1
    b.ne fail
    mov w1, #'P'
    str w1, [x20]
    mov w1, #10
    str w1, [x20]
    mov x0, #8
    movk x0, #0x8400, lsl #16
    hvc #0
fail:
    mov w1, #'F'
    str w1, [x20]
    mov w1, #10
    str w1, [x20]
    mov x0, #8
    movk x0, #0x8400, lsl #16
    hvc #0
secondary:
    mov x20, #0x09000000
    mov x1, #0x1234
    cmp x0, x1
    b.ne fail
    mov w1, #'S'
    str w1, [x20]
    mov w1, #10
    str w1, [x20]
    mov w1, #1
    adr x21, flag
    stlr w1, [x21]
    mov x0, #2
    movk x0, #0x8400, lsl #16
    hvc #0
    b fail
.balign 8
flag:
    .quad 0
