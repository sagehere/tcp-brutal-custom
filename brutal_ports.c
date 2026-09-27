// Local TCP service port rules. Selection happens when Brutal is initialized
// on a passive connection; a cgroup sockops hook chooses the algorithm.
#include <linux/inet.h>
#include <linux/jiffies.h>
#include <linux/mutex.h>
#include <linux/proc_fs.h>
#include <linux/random.h>
#include <linux/rculist.h>
#include <linux/seq_file.h>
#include <linux/slab.h>
#include <net/net_namespace.h>
#include "brutal.h"

struct brutal_port_rule
{
    struct list_head list;
    u16 port;
    struct brutal_group *group;
    unsigned long retired_at;
};

static LIST_HEAD(active_ports);
static LIST_HEAD(retired_ports);
static DEFINE_MUTEX(port_mutex);
static u64 port_next_id;

void brutal_apply_port(struct sock *sk, struct brutal *brutal)
{
    struct brutal_port_rule *r;
    u16 port;

    if (!net_eq(sock_net(sk), &init_net))
        return;
    port = inet_sk(sk)->inet_num;
    if (!port)
        return;
    rcu_read_lock();
    list_for_each_entry_rcu(r, &active_ports, list)
    {
        if (r->port == port)
        {
            refcount_inc(&r->group->refcnt);
            brutal_group_join(brutal, r->group);
            break;
        }
    }
    rcu_read_unlock();
}

// Caller holds port_mutex. Retired groups remain visible while sockets use them.
static void reap_ports(void)
{
    struct brutal_port_rule *r, *tmp;

    list_for_each_entry_safe(r, tmp, &retired_ports, list)
    {
        // Allow the 10-second sampler to observe final socket counters.
        if (READ_ONCE(r->group->members) ||
            time_before(jiffies, r->retired_at + 60 * HZ))
            continue;
        list_del_rcu(&r->list);
        synchronize_rcu();
        brutal_group_put(r->group);
        kfree(r);
    }
}

static int port_add(char *args)
{
    struct brutal_port_rule *r;
    struct brutal_group *g;
    char *tok;
    unsigned int port, gain = INIT_CWND_GAIN;
    u64 rate = 0;
    int ret;

    tok = strsep(&args, " ");
    if (!tok || kstrtouint(tok, 10, &port) || !port || port > 65535)
        return -EINVAL;
    while ((tok = strsep(&args, " ")))
    {
        if (!*tok)
            continue;
        if (!strncmp(tok, "rate=", 5))
            ret = kstrtou64(tok + 5, 10, &rate);
        else if (!strncmp(tok, "gain=", 5))
            ret = kstrtouint(tok + 5, 10, &gain);
        else
            ret = -EINVAL;
        if (ret)
            return -EINVAL;
    }
    if (rate < MIN_PACING_RATE || rate > MAX_PACING_RATE ||
        gain < MIN_CWND_GAIN || gain > MAX_CWND_GAIN)
        return -EINVAL;

    mutex_lock(&port_mutex);
    reap_ports();
    list_for_each_entry(r, &active_ports, list)
    {
        if (r->port == port)
        {
            WRITE_ONCE(r->group->rate, rate);
            WRITE_ONCE(r->group->cwnd_gain, gain);
            mutex_unlock(&port_mutex);
            return 0;
        }
    }
    r = kzalloc(sizeof(*r), GFP_KERNEL);
    g = r ? brutal_group_alloc(++port_next_id) : NULL;
    if (!g)
    {
        kfree(r);
        mutex_unlock(&port_mutex);
        return -ENOMEM;
    }
    r->port = port;
    r->group = g;
    g->rate = rate;
    g->cwnd_gain = gain;
    g->locked = 1;
    list_add_tail_rcu(&r->list, &active_ports);
    mutex_unlock(&port_mutex);
    return 0;
}

static int port_del(char *args)
{
    struct brutal_port_rule *r;
    unsigned int port;

    if (!args || kstrtouint(args, 10, &port) || !port || port > 65535)
        return -EINVAL;
    mutex_lock(&port_mutex);
    list_for_each_entry(r, &active_ports, list)
    {
        if (r->port == port)
        {
            list_del_rcu(&r->list);
            synchronize_rcu();
            r->retired_at = jiffies;
            list_add_tail_rcu(&r->list, &retired_ports);
            reap_ports();
            mutex_unlock(&port_mutex);
            return 0;
        }
    }
    mutex_unlock(&port_mutex);
    return -ENOENT;
}

static void show_port(struct seq_file *m, const struct brutal_port_rule *r, bool active)
{
    const struct brutal_group *g = r->group;

    seq_printf(m, "port=%u active=%u rate=%llu gain=%u id=%llu members=%u "
               "sent=%llu acked=%llu retrans=%llu rtt_sum=%llu "
               "rtt_samples=%llu rtt_max=%u\n",
               r->port, active, READ_ONCE(g->rate), READ_ONCE(g->cwnd_gain),
               g->id, READ_ONCE(g->members), READ_ONCE(g->sent_bytes),
               READ_ONCE(g->acked_bytes), READ_ONCE(g->retrans_bytes),
               READ_ONCE(g->rtt_sum_us), READ_ONCE(g->rtt_samples),
               READ_ONCE(g->rtt_max_us));
}

static int ports_show(struct seq_file *m, void *v)
{
    struct brutal_port_rule *r;

    mutex_lock(&port_mutex);
    reap_ports();
    list_for_each_entry(r, &active_ports, list)
        show_port(m, r, true);
    list_for_each_entry(r, &retired_ports, list)
        show_port(m, r, false);
    mutex_unlock(&port_mutex);
    return 0;
}

static int ports_open(struct inode *inode, struct file *file)
{
    return single_open(file, ports_show, NULL);
}

static ssize_t ports_write(struct file *file, const char __user *ubuf,
                           size_t len, loff_t *off)
{
    char *buf, *args, *cmd;
    int ret;

    if (!len || len > 128)
        return -EINVAL;
    buf = memdup_user_nul(ubuf, len);
    if (IS_ERR(buf))
        return PTR_ERR(buf);
    args = strim(buf);
    cmd = strsep(&args, " ");
    if (!strcmp(cmd, "add"))
        ret = port_add(args);
    else if (!strcmp(cmd, "del"))
        ret = port_del(args);
    else
        ret = -EINVAL;
    kfree(buf);
    return ret ?: len;
}

static const struct proc_ops ports_ops = {
    .proc_open = ports_open,
    .proc_read = seq_read,
    .proc_write = ports_write,
    .proc_lseek = seq_lseek,
    .proc_release = single_release,
};

int brutal_ports_init(struct proc_dir_entry *dir)
{
    port_next_id = ((u64)(get_random_u32() & 0x3fffffff) << 32) | 0x100000000ULL;
    return proc_create("ports", 0600, dir, &ports_ops) ? 0 : -ENOMEM;
}

void brutal_ports_exit(void)
{
    struct brutal_port_rule *r, *tmp;

    mutex_lock(&port_mutex);
    list_for_each_entry_safe(r, tmp, &active_ports, list)
    {
        list_del(&r->list);
        brutal_group_put(r->group);
        kfree(r);
    }
    list_for_each_entry_safe(r, tmp, &retired_ports, list)
    {
        list_del(&r->list);
        brutal_group_put(r->group);
        kfree(r);
    }
    mutex_unlock(&port_mutex);
}
