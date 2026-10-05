#ifndef BRUTAL_CLOCK_H
#define BRUTAL_CLOCK_H

// Keep refunds bounded by available credit, and debts within the pacing horizon.
static inline u64 brutal_clock_adjust(u64 next, u64 now, u64 amount, bool refund)
{
    u64 floor = now > 2000000 ? now - 2000000 : 0;
    u64 ceiling = now > U64_MAX - 1000000000 ? U64_MAX : now + 1000000000;

    next = clamp_t(u64, next, floor, ceiling);
    if (refund)
        return next - min(amount, next - floor);
    return next + min(amount, ceiling - next);
}

static inline u64 brutal_clock_settle(u64 next, u64 now, u64 reserved, u64 sent, u64 rate)
{
    bool refund = sent < reserved;
    u64 difference = refund ? reserved - sent : sent - reserved;
    u64 duration = mul_u64_u64_div_u64(difference, 1000000000, max_t(u64, rate, 1));

    return brutal_clock_adjust(next, now, duration, refund);
}

#endif
