// The congestion control: rate and cwnd, loss compensation, and the group clock
#include <linux/module.h>
#include <linux/math64.h>
#include <linux/slab.h>
#include "brutal.h"

#define MIN_PKT_INFO_SAMPLES 50
#define MIN_ACK_RATE_PERCENT 80

// Phase-one guardrails: keep the useful random-loss behavior without letting
// an oversized configured rate amplify queue congestion.
//
// Non-congested loss compensation may raise pacing by at most 10%.
#define MAX_LOSS_COMPENSATION_PERCENT 110
// Ignore tiny loss levels when deciding whether queue growth is congestion.
#define CONGESTION_LOSS_PERCENT 2
// Suppress loss compensation once SRTT reaches 1.25x the minimum observed SRTT.
#define CONGESTION_RTT_PERCENT 125

// Phase-two per-socket adaptive ceiling. Delivery rate is measured over a
// relatively long interval after loss so ACK compression and initial policer
// token bursts do not dominate the estimate.
#define ADAPTIVE_SAMPLE_INTERVAL_US (500 * USEC_PER_MSEC)
#define ADAPTIVE_TRIGGER_RATE_PERCENT 80
#define ADAPTIVE_LOW_UTIL_PERCENT 80
#define ADAPTIVE_HEADROOM_PERCENT 120
#define ADAPTIVE_PROBE_PERCENT 125
#define ADAPTIVE_PROBE_INTERVAL_US (1 * USEC_PER_SEC)
#define ADAPTIVE_PROBE_HOLD_US (500 * USEC_PER_MSEC)
#define ADAPTIVE_PROBE_UTIL_PERCENT 85
#define ADAPTIVE_CONFIRM_SAMPLES 2
#define ADAPTIVE_NOLOSS_CONFIRM_SAMPLES 4

// An unused reserved slot is returned to the group this long after its time
#define RESV_STALE_NS (20 * NSEC_PER_MSEC)
// Max lag of the group clock behind real time (token bucket depth)
#define GROUP_MAX_LAG_NS (2 * NSEC_PER_MSEC)

static u64 brutal_configured_rate(const struct brutal *brutal)
{
    return brutal->group ? READ_ONCE(brutal->group->rate) : brutal->rate;
}

// Configured rate compensated for random loss, before any phase-two per-socket
// adaptive ceiling. The group clock uses this rate so one slow socket does not
// lower the budget available to the other members.
static u64 brutal_compensated_rate(const struct brutal *brutal)
{
    u64 rate = brutal_configured_rate(brutal);
    u64 compensated, ceiling;

    // Loss plus queue growth is treated as congestion, not random loss. In
    // that state keep pacing at the configured rate instead of adding traffic.
    if (brutal->stats && READ_ONCE(brutal->stats->congestion_limited))
        return rate;

    compensated = div_u64(rate * 100, brutal->ack_rate);
    ceiling = div_u64(rate * MAX_LOSS_COMPENSATION_PERCENT, 100);
    return min_t(u64, compensated, ceiling);
}

static u64 brutal_socket_rate(const struct brutal *brutal)
{
    u64 rate = brutal_compensated_rate(brutal);
    u64 adaptive;

    if (!brutal->stats)
        return rate;
    adaptive = READ_ONCE(brutal->stats->adaptive_ceiling);
    return adaptive ? min(rate, adaptive) : rate;
}

// Account from TCP's own payload counters. The last flush is on CA release,
// so short connections and the final burst are included.
void brutal_stats_flush(struct sock *sk)
{
    struct brutal *brutal = inet_csk_ca(sk);
    struct brutal_stats_state *st = brutal->stats;
    struct brutal_group *g = brutal->group;
    struct tcp_sock *tp = tcp_sk(sk);
    u64 sent, acked, retrans;
    u32 rtt;

    if (!g || !st)
        return;
    sent = tp->bytes_sent;
    acked = tp->bytes_acked;
    retrans = tp->bytes_retrans;
    rtt = tp->srtt_us >> 3;
    spin_lock_bh(&g->lock);
    g->sent_bytes += sent - st->sent;
    g->acked_bytes += acked - st->acked;
    g->retrans_bytes += retrans - st->retrans;
    if (rtt)
    {
        g->rtt_sum_us += rtt;
        g->rtt_samples++;
        g->rtt_max_us = max(g->rtt_max_us, rtt);
    }
    spin_unlock_bh(&g->lock);
    st->sent = sent;
    st->acked = acked;
    st->retrans = retrans;
}

static void brutal_adaptive_sample_start(struct tcp_sock *tp, struct brutal_stats_state *st)
{
    st->sample_start_acked = tp->bytes_acked;
    st->sample_start_us = tp->tcp_mstamp;
    st->sample_active = 1;
}

static void brutal_update_adaptive(struct sock *sk, const struct rate_sample *rs)
{
    struct tcp_sock *tp = tcp_sk(sk);
    struct brutal *brutal = inet_csk_ca(sk);
    struct brutal_stats_state *st = brutal->stats;
    u64 configured, now, elapsed, acked, delivery_rate, target, ceiling;
    bool loss_signal, mismatch, group_safe, low_loss_policer, no_loss_mismatch;
    u32 members = 1, confirm_samples;

    if (!st || !rs || rs->delivered <= 0 || rs->interval_us <= 0)
        return;

    now = tp->tcp_mstamp;
    configured = brutal_configured_rate(brutal);

    // An application-limited interval is not evidence of path capacity.
    if (rs->is_app_limited)
    {
        st->sample_active = 0;
        return;
    }

    ceiling = READ_ONCE(st->adaptive_ceiling);

    // A capped socket periodically probes upward. During the hold interval,
    // mismatch-only evidence cannot immediately undo the probe; clear RTT
    // congestion still can.
    if (ceiling && !READ_ONCE(st->congestion_limited) &&
        now - st->last_probe_us >= ADAPTIVE_PROBE_INTERVAL_US &&
        (!st->last_delivery_rate ||
         st->last_delivery_rate * 100 >= ceiling * ADAPTIVE_PROBE_UTIL_PERCENT))
    {
        u64 probed = div_u64(ceiling * ADAPTIVE_PROBE_PERCENT, 100);

        if (probed >= configured)
            WRITE_ONCE(st->adaptive_ceiling, 0);
        else
            WRITE_ONCE(st->adaptive_ceiling, max(probed, ceiling + 1));
        st->last_probe_us = now;
        st->candidate_ceiling = 0;
        st->candidate_samples = 0;
        ceiling = READ_ONCE(st->adaptive_ceiling);
    }

    loss_signal = rs->losses || st->recent_losses ||
                  st->recent_loss_percent >= CONGESTION_LOSS_PERCENT ||
                  READ_ONCE(st->congestion_limited);
    if (!st->sample_active)
    {
        // Keep a low-frequency capacity sample running for a single active
        // member even when TCP itself has not reported loss. Some policers
        // discard below TCP's immediate loss-accounting horizon, so loss-only
        // sampling can miss a persistent configured/delivered mismatch.
        if (loss_signal || !brutal->group || READ_ONCE(brutal->group->members) <= 1)
            brutal_adaptive_sample_start(tp, st);
        return;
    }

    if (now <= st->sample_start_us)
    {
        brutal_adaptive_sample_start(tp, st);
        return;
    }
    elapsed = now - st->sample_start_us;
    if (elapsed < ADAPTIVE_SAMPLE_INTERVAL_US)
        return;

    acked = tp->bytes_acked - st->sample_start_acked;
    st->sample_active = 0;
    if (!acked)
        return;

    delivery_rate = mul_u64_u64_div_u64(acked, USEC_PER_SEC, elapsed);
    delivery_rate = min_t(u64, delivery_rate, MAX_PACING_RATE);
    st->last_delivery_rate = delivery_rate;

    mismatch = delivery_rate * 100 < configured * ADAPTIVE_TRIGGER_RATE_PERCENT;
    if (brutal->group)
        members = READ_ONCE(brutal->group->members);
    group_safe = members <= 1 || READ_ONCE(st->congestion_limited);

    // A token-bucket policer can hold a path far below the configured rate
    // while producing less than the 2% loss used by the phase-one RTT
    // congestion guard. For a single active member, any real packet loss plus
    // a persistent delivery mismatch is enough evidence to run the adaptive
    // ceiling confirmation. With multiple group members we keep the stricter
    // congestion requirement so ordinary group sharing is never mistaken for
    // a per-client bottleneck.
    low_loss_policer = members <= 1 && st->recent_losses &&
                       ((!ceiling && mismatch) ||
                        (ceiling && delivery_rate * 100 < ceiling * ADAPTIVE_LOW_UTIL_PERCENT));
    no_loss_mismatch = members <= 1 && !st->recent_losses &&
                       !READ_ONCE(st->congestion_limited) &&
                       ((!ceiling && mismatch) ||
                        (ceiling && delivery_rate * 100 < ceiling * ADAPTIVE_LOW_UTIL_PERCENT));
    confirm_samples = no_loss_mismatch ? ADAPTIVE_NOLOSS_CONFIRM_SAMPLES : ADAPTIVE_CONFIRM_SAMPLES;

    if (group_safe &&
        (st->recent_loss_percent >= CONGESTION_LOSS_PERCENT ||
         READ_ONCE(st->congestion_limited) || low_loss_policer || no_loss_mismatch) &&
        ((!ceiling && mismatch) || READ_ONCE(st->congestion_limited) ||
         (ceiling && delivery_rate * 100 < ceiling * ADAPTIVE_LOW_UTIL_PERCENT)))
    {
        bool probe_hold = ceiling && now - st->last_probe_us < ADAPTIVE_PROBE_HOLD_US;

        if (!probe_hold || READ_ONCE(st->congestion_limited))
        {
            // Convert unique delivered bytes back to an estimated wire-rate
            // need using the observed loss, then keep 20% adaptive headroom.
            // This avoids under-driving lossy links (for example, 5% random
            // loss would otherwise turn a 100 Mbps path into an ~90 Mbps cap).
            target = div_u64(delivery_rate * 100,
                             max_t(u32, 100 - st->recent_loss_percent, MIN_ACK_RATE_PERCENT));
            target = div_u64(target * ADAPTIVE_HEADROOM_PERCENT, 100);
            target = clamp_t(u64, target, MIN_PACING_RATE, configured);

            // Never let one unlucky 500 ms ACK/loss window determine the
            // long-lived pacing ceiling. Require two consecutive downward
            // samples and use the higher estimate from those samples.
            if (!st->candidate_samples)
                st->candidate_ceiling = target;
            else
                st->candidate_ceiling = max(st->candidate_ceiling, target);
            st->candidate_samples++;

            if (st->candidate_samples >= confirm_samples)
            {
                ceiling = READ_ONCE(st->adaptive_ceiling);
                if (!ceiling || st->candidate_ceiling < ceiling)
                    WRITE_ONCE(st->adaptive_ceiling, st->candidate_ceiling);
                st->last_probe_us = now;
                st->candidate_ceiling = 0;
                st->candidate_samples = 0;
            }
        }
    }
    else
    {
        st->candidate_ceiling = 0;
        st->candidate_samples = 0;
    }

    if (loss_signal || members <= 1)
        brutal_adaptive_sample_start(tp, st);
}

void brutal_update_rate(struct sock *sk, const struct rate_sample *rs)
{
    struct tcp_sock *tp = tcp_sk(sk);
    struct brutal *brutal = inet_csk_ca(sk);

    u32 sec = div_u64(tp->tcp_mstamp, USEC_PER_SEC);
    u32 min_sec = sec - PKT_INFO_SLOTS;
    u32 acked = 0, losses = 0;
    u32 ack_rate; // Scaled by 100 (100=1.00) as kernel doesn't support float
    u64 rate, bdp, cwnd;
    u32 cwnd_gain, rtt_us, base_rtt_us, samples;

    for (int i = 0; i < PKT_INFO_SLOTS; i++)
    {
        if (brutal->slots[i].sec >= min_sec)
        {
            acked += brutal->slots[i].acked;
            losses += brutal->slots[i].losses;
        }
    }
    samples = acked + losses;
    if (samples < MIN_PKT_INFO_SAMPLES)
        ack_rate = 100;
    else
    {
        ack_rate = acked * 100 / samples;
        if (ack_rate < MIN_ACK_RATE_PERCENT)
            ack_rate = MIN_ACK_RATE_PERCENT;
    }
    brutal->ack_rate = ack_rate;
    if (brutal->stats)
    {
        brutal->stats->recent_losses = losses;
        brutal->stats->recent_loss_percent =
            samples ? min_t(u64, (u64)losses * 100 / samples, 100) : 0;
    }

    rtt_us = tp->srtt_us >> 3;
    base_rtt_us = rtt_us;
    if (brutal->stats && rtt_us)
    {
        if (!brutal->stats->min_rtt_us || rtt_us < brutal->stats->min_rtt_us)
            brutal->stats->min_rtt_us = rtt_us;
        base_rtt_us = brutal->stats->min_rtt_us;

        // Random loss alone may still be compensated. Suppress compensation
        // only when loss is accompanied by clear queue/RTT inflation.
        WRITE_ONCE(brutal->stats->congestion_limited,
                   samples >= MIN_PKT_INFO_SAMPLES &&
                       (u64)losses * 100 >= (u64)samples * CONGESTION_LOSS_PERCENT &&
                       (u64)rtt_us * 100 >= (u64)base_rtt_us * CONGESTION_RTT_PERCENT);
    }

    brutal_update_adaptive(sk, rs);

    rate = brutal_socket_rate(brutal);
    cwnd_gain = brutal->group ? READ_ONCE(brutal->group->cwnd_gain) : brutal->cwnd_gain;

    // Size inflight from the minimum observed SRTT rather than the current
    // queue-inflated SRTT. This avoids growing cwnd merely because a bottleneck
    // queue has already built up.
    bdp = mul_u64_u64_div_u64(rate, max_t(u32, base_rtt_us, USEC_PER_MSEC), USEC_PER_SEC);
    cwnd = div_u64(bdp * cwnd_gain, 10 * tp->mss_cache);

    // In a group, cwnd and sk_pacing_rate are sized for the full group rate so
    // that a member can take all of it at any moment; the group clock decides
    // the actual share.
    tp->snd_cwnd = clamp_t(u64, cwnd, MIN_CWND, min_t(u32, tp->snd_cwnd_clamp, INT_MAX));

    WRITE_ONCE(sk->sk_pacing_rate, min_t(u64, rate, READ_ONCE(sk->sk_max_pacing_rate)));
}

// Segments tcp_write_xmit puts in one skb at this rate (mirrors tcp_tso_autosize)
static u32 brutal_tso_segs_estimate(const struct sock *sk, u64 rate, u32 mss)
{
    unsigned long bytes = rate >> READ_ONCE(sk->sk_pacing_shift);

#if LINUX_VERSION_CODE >= KERNEL_VERSION(5, 18, 0)
    u32 r = tcp_min_rtt(tcp_sk(sk)) >> READ_ONCE(sock_net(sk)->ipv4.sysctl_tcp_tso_rtt_log);
    if (r < BITS_PER_TYPE(sk->sk_gso_max_size))
        bytes += sk->sk_gso_max_size >> r;
#endif
    bytes = min_t(unsigned long, bytes, sk->sk_gso_max_size);
    return clamp_t(u32, bytes / mss, 2, sk->sk_gso_max_segs);
}

// Bytes tcp_write_xmit is about to send in one go
static u32 brutal_burst_estimate(const struct sock *sk, u64 rate, u32 unsent)
{
    const struct tcp_sock *tp = tcp_sk(sk);
    u32 segs = brutal_tso_segs_estimate(sk, rate, tp->mss_cache);

    segs = min(segs, tp->snd_cwnd - tcp_packets_in_flight(tp));
    unsent = min(unsent, tcp_wnd_end(tp) - tp->snd_nxt);
    return min_t(u32, segs * tp->mss_cache, unsent);
}

// Called from the TSO hook, which the kernel invokes once at the start of every
// tcp_write_xmit / tcp_xmit_retransmit_queue, under the socket lock, before it
// checks pacing. This is where a group member claims its slot on the group clock.
static void brutal_group_reserve(struct sock *sk)
{
    struct tcp_sock *tp = tcp_sk(sk);
    struct brutal *brutal = inet_csk_ca(sk);
    struct brutal_group *g = brutal->group;
    u64 now = tp->tcp_clock_cache;
    u64 group_rate, socket_rate, start;
    u32 unsent, burst;

    if (!g)
        return;

    group_rate = brutal_compensated_rate(brutal);
    socket_rate = brutal_socket_rate(brutal);

    // Settle the previous reservation against what was actually sent
    if (brutal->resv_bytes)
    {
        u64 sent = tp->bytes_sent - brutal->resv_bytes_sent;
        s64 delta;

        if (!sent && (s64)(now - brutal->resv_start_ns) < (s64)RESV_STALE_NS)
        {
            // Pacing timer wake-up (or a blocked send): the slot is still ours
            if (tp->tcp_wstamp_ns < brutal->resv_start_ns)
                tp->tcp_wstamp_ns = brutal->resv_start_ns;
            return;
        }
        delta = (s64)sent - (s64)brutal->resv_bytes; // < 0: give time back
        spin_lock_bh(&g->lock);
        if (delta >= 0)
            g->next_ns += div64_u64((u64)delta * NSEC_PER_SEC, group_rate);
        else
            g->next_ns -= div64_u64((u64)(-delta) * NSEC_PER_SEC, group_rate);
        spin_unlock_bh(&g->lock);
        brutal->resv_bytes = 0;
    }

    brutal_stats_flush(sk);

    // Reserve the next burst, if this call can actually send one
    unsent = tp->write_seq - tp->snd_nxt;
    if (!unsent)
    {
        if (tp->lost_out <= tp->retrans_out)
            return;
        unsent = tp->mss_cache; // retransmission pending
    }
    if (tcp_packets_in_flight(tp) >= tp->snd_cwnd || !after(tcp_wnd_end(tp), tp->snd_nxt))
        return;

    burst = brutal_burst_estimate(sk, socket_rate, unsent);

    spin_lock_bh(&g->lock);
    start = max(g->next_ns, now - GROUP_MAX_LAG_NS);
    g->next_ns = start + div64_u64((u64)burst * NSEC_PER_SEC, group_rate);
    spin_unlock_bh(&g->lock);

    brutal->resv_start_ns = start;
    brutal->resv_bytes = burst;
    brutal->resv_bytes_sent = tp->bytes_sent;
    if (tp->tcp_wstamp_ns < start)
        tp->tcp_wstamp_ns = start;
}

#ifdef BRUTAL_HAVE_TSO_SEGS
// Kernels with the BBRv3 patchset (XanMod and others): tso_segs replaces
// tcp_tso_autosize, so the return value is the burst size, not a floor.
static u32 brutal_tso_segs(struct sock *sk, unsigned int mss_now)
{
    brutal_group_reserve(sk);
    return brutal_tso_segs_estimate(sk, READ_ONCE(sk->sk_pacing_rate), mss_now);
}
#else
static u32 brutal_min_tso_segs(struct sock *sk)
{
    brutal_group_reserve(sk);
    return 2;
}
#endif

static void brutal_init(struct sock *sk)
{
    struct tcp_sock *tp = tcp_sk(sk);
    struct brutal *brutal = inet_csk_ca(sk);

    brutal_sockopt_install(sk);

    tp->snd_ssthresh = TCP_INFINITE_SSTHRESH;

    memset(brutal, 0, sizeof(*brutal));
    brutal->rate = INIT_PACING_RATE;
    brutal->cwnd_gain = INIT_CWND_GAIN;
    brutal->ack_rate = 100;
    brutal->stats = kzalloc(sizeof(*brutal->stats), GFP_ATOMIC);
    if (brutal->stats)
    {
        brutal->stats->sent = tp->bytes_sent;
        brutal->stats->acked = tp->bytes_acked;
        brutal->stats->retrans = tp->bytes_retrans;
        brutal->stats->min_rtt_us = tp->srtt_us >> 3;
    }

    brutal_apply_port(sk, brutal);
    if (!brutal->group)
        brutal_apply_rule(sk, brutal);
    if (brutal->group)
        brutal_update_rate(sk, NULL);

    // Pacing is REQUIRED for Brutal to work
    cmpxchg(&sk->sk_pacing_status, SK_PACING_NONE, SK_PACING_NEEDED);
}

static void brutal_release(struct sock *sk)
{
    struct brutal *brutal = inet_csk_ca(sk);

    brutal_group_leave(sk, brutal);
    kfree(brutal->stats);
    brutal_sockopt_uninstall(sk);
}

#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 10, 0)
static void brutal_main(struct sock *sk, u32 ack, int flag, const struct rate_sample *rs)
#else
static void brutal_main(struct sock *sk, const struct rate_sample *rs)
#endif
{
    struct tcp_sock *tp = tcp_sk(sk);
    struct brutal *brutal = inet_csk_ca(sk);

    u32 sec, slot;

    // Ignore invalid rate samples
    if (rs->delivered < 0 || rs->interval_us <= 0)
        return;

    sec = div_u64(tp->tcp_mstamp, USEC_PER_SEC);
    slot = sec % PKT_INFO_SLOTS;

    if (brutal->slots[slot].sec == sec)
    {
        // Current slot, update
        brutal->slots[slot].acked += rs->acked_sacked;
        brutal->slots[slot].losses += rs->losses;
    }
    else
    {
        // Uninitialized slot or slot expired
        brutal->slots[slot].sec = sec;
        brutal->slots[slot].acked = rs->acked_sacked;
        brutal->slots[slot].losses = rs->losses;
    }

    brutal_update_rate(sk, rs);
    brutal_stats_flush(sk);
}

static u32 brutal_undo_cwnd(struct sock *sk)
{
    return tcp_sk(sk)->snd_cwnd;
}

static u32 brutal_ssthresh(struct sock *sk)
{
    return tcp_sk(sk)->snd_ssthresh;
}

struct tcp_congestion_ops tcp_brutal_ops = {
    .flags = TCP_CONG_NON_RESTRICTED,
    .name = "brutal",
    .owner = THIS_MODULE,
    .init = brutal_init,
    .release = brutal_release,
    .cong_control = brutal_main,
    .undo_cwnd = brutal_undo_cwnd,
    .ssthresh = brutal_ssthresh,
#ifdef BRUTAL_HAVE_TSO_SEGS
    .tso_segs = brutal_tso_segs,
#else
    .min_tso_segs = brutal_min_tso_segs,
#endif
};

static int __init brutal_register(void)
{
    int ret;

    BUILD_BUG_ON(sizeof(struct brutal) > ICSK_CA_PRIV_SIZE);
    BUILD_BUG_ON(sizeof(struct brutal_params) != 20);

    brutal_sockopt_init();
    ret = brutal_rules_init();
    if (ret)
        return ret;
    ret = tcp_register_congestion_control(&tcp_brutal_ops);
    if (ret)
        brutal_rules_exit();
    return ret;
}

static void __exit brutal_unregister(void)
{
    tcp_unregister_congestion_control(&tcp_brutal_ops);
    brutal_rules_exit();
}

module_init(brutal_register);
module_exit(brutal_unregister);

MODULE_AUTHOR("The Hysteria Project");
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("TCP Brutal");
MODULE_VERSION(__stringify(BRUTAL_VERSION_MAJOR) "." __stringify(BRUTAL_VERSION_MINOR) "." __stringify(BRUTAL_VERSION_PATCH));
