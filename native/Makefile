SDK := $(shell xcrun --sdk macosx --show-sdk-path)
CC := xcrun --sdk macosx clang
CPPFLAGS := -I.
CFLAGS := -std=c11 -O2 -g -Wall -Wextra -Werror -Wpedantic \
	-isysroot $(SDK) -mmacosx-version-min=15.0 -arch arm64 -pthread
LDLIBS := -framework Hypervisor
COMMON := memory.c fdt.c virtqueue.c devices.c rtc.c vsock.c io.c reset.c diagnostics.c
SOURCES := main.c hypervisor.c $(COMMON)
OBJECTS := $(SOURCES:%.c=build/%.o)

.PHONY: all clean test probe sanitize smoke memory-test
all: build/sapphire-vmm

build:
	mkdir -p $@

build/%.o: %.c vmm.h | build
	$(CC) $(CPPFLAGS) $(CFLAGS) -MMD -MP -c $< -o $@

build/sapphire-vmm: $(OBJECTS) entitlements.plist
	$(CC) $(CFLAGS) $(OBJECTS) $(LDLIBS) -o $@.tmp
	codesign --force --sign - --entitlements entitlements.plist $@.tmp
	mv -f $@.tmp $@

build/native-tests: tests/test.c $(COMMON) vmm.h | build
	$(CC) $(CPPFLAGS) $(CFLAGS) tests/test.c $(COMMON) -o $@

build/memory-churn: tests/memory_churn.c hypervisor.c $(COMMON) vmm.h entitlements.plist | build
	$(CC) $(CPPFLAGS) $(CFLAGS) tests/memory_churn.c hypervisor.c $(COMMON) $(LDLIBS) -o $@
	codesign --force --sign - --entitlements entitlements.plist $@

test: build/native-tests
	./build/native-tests

memory-test: build/memory-churn
	./build/memory-churn 512 6

sanitize: | build
	$(CC) $(CPPFLAGS) $(CFLAGS) -O1 -fsanitize=address,undefined \
		tests/test.c $(COMMON) -o build/native-tests-sanitize
	UBSAN_OPTIONS=halt_on_error=1 ./build/native-tests-sanitize

probe: build/sapphire-vmm
	./build/sapphire-vmm --probe

smoke: build/sapphire-vmm
	python3 tests/smoke.py

clean:
	rm -rf build

-include $(OBJECTS:.o=.d)
