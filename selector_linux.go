//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

type selectorCount struct {
	Baseline  uint64 `json:"baseline"`
	Canary    uint64 `json:"canary"`
	Failure   uint64 `json:"failure"`
	LastError int32  `json:"last_error"`
	_         uint32
}

func (c selectorCount) Success() uint64 { return c.Baseline + c.Canary }

type selector struct {
	ports   *ebpf.Map
	counts  *ebpf.Map
	host    *ebpf.Map
	program *ebpf.Program
	link    link.Link
}

func newSelector() (_ *selector, err error) {
	s := &selector{}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	s.ports, err = ebpf.NewMap(&ebpf.MapSpec{Name: "btc_ports", Type: ebpf.Hash, KeySize: 4, ValueSize: 4, MaxEntries: 1024})
	if err != nil {
		return nil, fmt.Errorf("port map: %w", err)
	}
	s.host, err = ebpf.NewMap(&ebpf.MapSpec{Name: "btc_netns", Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		return nil, fmt.Errorf("netns map: %w", err)
	}
	s.counts, err = ebpf.NewMap(&ebpf.MapSpec{Name: "btc_counts", Type: ebpf.Array, KeySize: 4, ValueSize: 32, MaxEntries: 65536})
	if err != nil {
		return nil, fmt.Errorf("counter map: %w", err)
	}
	fd, e := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if e != nil {
		return nil, e
	}
	cookie, e := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_NETNS_COOKIE)
	unix.Close(fd)
	if e != nil {
		return nil, fmt.Errorf("SO_NETNS_COOKIE: %w", e)
	}
	var zero uint32
	if err = s.host.Put(&zero, &cookie); err != nil {
		return nil, err
	}
	s.program, err = ebpf.NewProgram(&ebpf.ProgramSpec{
		Name: "btc_port_select", Type: ebpf.SockOps, AttachType: ebpf.AttachCGroupSockOps,
		License: "GPL", Instructions: s.instructions(),
	})
	if err != nil {
		return nil, fmt.Errorf("sockops program: %w", err)
	}
	s.link, err = link.AttachCgroup(link.CgroupOptions{Path: "/sys/fs/cgroup", Attach: ebpf.AttachCGroupSockOps, Program: s.program})
	if err != nil {
		return nil, fmt.Errorf("cgroup attach: %w", err)
	}
	return s, nil
}

// The hook only selects the algorithm. The kernel module owns the port's
// group and parameters; a failure leaves the original algorithm unchanged.
func (s *selector) instructions() asm.Instructions {
	return asm.Instructions{
		asm.LoadMem(asm.R0, asm.R1, 0, asm.Word),
		asm.JNE.Imm(asm.R0, 5, "exit"), // BPF_SOCK_OPS_PASSIVE_ESTABLISHED_CB
		asm.Mov.Reg(asm.R6, asm.R1),
		asm.FnGetNetnsCookie.Call(),
		asm.Mov.Reg(asm.R7, asm.R0),
		asm.StoreImm(asm.RFP, -4, 0, asm.Word),
		asm.LoadMapPtr(asm.R1, s.host.FD()),
		asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -4),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.LoadMem(asm.R1, asm.R0, 0, asm.DWord),
		asm.JNE.Reg(asm.R1, asm.R7, "exit"),

		asm.LoadMem(asm.R7, asm.R6, 68, asm.Word), // local_port, host byte order
		asm.StoreMem(asm.RFP, -8, asm.R7, asm.Word),
		asm.LoadMapPtr(asm.R1, s.ports.FD()),
		asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -8),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.LoadMem(asm.R8, asm.R0, 0, asm.Word), // Canary percentage 0..100
		asm.JGT.Imm(asm.R8, 100, "exit"),

		// Stable per-connection bucket. Existing connections never change
		// algorithm when the configured percentage is updated.
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.FnGetSocketCookie.Call(),
		asm.Mod.Imm(asm.R0, 100),
		asm.JLT.Reg(asm.R0, asm.R8, "choose_canary"),

		// Baseline: "brutal\0"
		asm.StoreImm(asm.RFP, -48, 0x74757262, asm.Word).WithSymbol("choose_baseline"),
		asm.StoreImm(asm.RFP, -44, 0x006c61, asm.Word),
		asm.StoreImm(asm.RFP, -12, 0, asm.Word),
		asm.Mov.Reg(asm.R4, asm.RFP), asm.Add.Imm(asm.R4, -48),
		asm.Mov.Imm(asm.R5, 7),
		asm.Ja.Label("set_cc"),

		// Canary: "brutal_adaptive\0"
		asm.StoreImm(asm.RFP, -48, 0x74757262, asm.Word).WithSymbol("choose_canary"),
		asm.StoreImm(asm.RFP, -44, 0x615f6c61, asm.Word),
		asm.StoreImm(asm.RFP, -40, 0x74706164, asm.Word),
		asm.StoreImm(asm.RFP, -36, 0x00657669, asm.Word),
		asm.StoreImm(asm.RFP, -12, 1, asm.Word),
		asm.Mov.Reg(asm.R4, asm.RFP), asm.Add.Imm(asm.R4, -48),
		asm.Mov.Imm(asm.R5, 16),

		asm.Mov.Reg(asm.R1, asm.R6).WithSymbol("set_cc"),
		asm.Mov.Imm(asm.R2, 6),  // IPPROTO_TCP
		asm.Mov.Imm(asm.R3, 13), // TCP_CONGESTION
		asm.FnSetsockopt.Call(),
		asm.Mov.Reg(asm.R9, asm.R0),

		asm.LoadMapPtr(asm.R1, s.counts.FD()),
		asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -8),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.Mov.Reg(asm.R8, asm.R0),
		asm.Mov.Imm(asm.R1, 1),
		asm.JNE.Imm(asm.R9, 0, "failed"),

		asm.LoadMem(asm.R7, asm.RFP, -12, asm.Word),
		asm.JEq.Imm(asm.R7, 0, "count_success"),
		asm.Add.Imm(asm.R8, 8), // Canary counter
		asm.StoreXAdd(asm.R8, asm.R1, asm.DWord).WithSymbol("count_success"),
		asm.Ja.Label("exit"),

		asm.StoreMem(asm.R8, 24, asm.R9, asm.Word).WithSymbol("failed"),
		asm.Add.Imm(asm.R8, 16), // Failure counter
		asm.StoreXAdd(asm.R8, asm.R1, asm.DWord),
		asm.Mov.Imm(asm.R0, 1).WithSymbol("exit"),
		asm.Return(),
	}
}

func (s *selector) Enable(port uint16, canaryPercent uint8) error {
	if canaryPercent > 100 {
		return fmt.Errorf("canary percentage out of range: %d", canaryPercent)
	}
	key, percent := uint32(port), uint32(canaryPercent)
	return s.ports.Put(&key, &percent)
}

func (s *selector) Disable(port uint16) error {
	key := uint32(port)
	if err := s.ports.Delete(&key); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return err
	}
	return nil
}

func (s *selector) Count(port uint16) (selectorCount, error) {
	var count selectorCount
	key := uint32(port)
	err := s.counts.Lookup(&key, &count)
	return count, err
}

func (s *selector) Close() {
	if s.link != nil {
		s.link.Close()
	}
	if s.program != nil {
		s.program.Close()
	}
	if s.ports != nil {
		s.ports.Close()
	}
	if s.host != nil {
		s.host.Close()
	}
	if s.counts != nil {
		s.counts.Close()
	}
}

func probeSelector() error {
	s, err := newSelector()
	if err != nil {
		return err
	}
	defer s.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err = s.Enable(port, 100); err != nil {
		return err
	}
	defer s.Disable(port)
	client, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		return err
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		return err
	}
	defer server.Close()
	count, err := s.Count(port)
	if err != nil {
		return err
	}
	if count.Success()+count.Failure == 0 {
		return fmt.Errorf("sockops hook did not inspect local port %d", port)
	}
	fmt.Printf("cgroup sockops verified: selected=%d failed=%d last_error=%d\n", count.Success(), count.Failure, count.LastError)
	return nil
}
