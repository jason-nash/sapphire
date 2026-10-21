.section .text
.global _start
_start:
    b primary
    .long 0
    .quad 0x80000
    .quad 0x10000
    .quad 0, 0, 0, 0
    .long 0x644d5241
    .long 0
primary:
    mov x20, #0x09000000
    mov w1, #'I'
    str w1, [x20]
    mov w1, #10
    str w1, [x20]
1:
    wfi
    b 1b
