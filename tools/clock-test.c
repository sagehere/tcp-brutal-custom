#include <assert.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>

typedef uint64_t u64;
#define U64_MAX UINT64_MAX
#define min(a, b) ((a) < (b) ? (a) : (b))
#define max_t(type, a, b) ((type)(a) > (type)(b) ? (type)(a) : (type)(b))
#define clamp_t(type, n, lo, hi) min(max_t(type, n, lo), hi)
static u64 mul_u64_u64_div_u64(u64 a, u64 b, u64 c)
{
    return (u64)((__uint128_t)a * b / c);
}
#include "../brutal_clock.h"

int main(void)
{
    u64 now = 10000000000ULL;
    u64 reserved_rate = 1000000;

    // A cancelled reservation refunds its original duration after a rate change.
    assert(brutal_clock_settle(now + 10000000, now, 10000, 0, reserved_rate) ==
           now);
    // Close after a partial send, or after sending more than was estimated.
    assert(brutal_clock_settle(now + 10000000, now, 10000, 5000, reserved_rate) ==
           now + 5000000);
    assert(brutal_clock_settle(now + 10000000, now, 10000, 15000,
                               reserved_rate) == now + 15000000);
    assert(brutal_clock_settle(now + 10000000, now, 10000, 10000,
                               reserved_rate) == now + 10000000);
    assert(brutal_clock_adjust(0, now, UINT64_MAX, true) == now - 2000000);
    assert(brutal_clock_adjust(UINT64_MAX, now, UINT64_MAX, false) ==
           now + 1000000000);
    assert(brutal_clock_adjust(0, 1, UINT64_MAX, true) == 0);
    puts("reservation clock regression checks passed");
    return 0;
}
