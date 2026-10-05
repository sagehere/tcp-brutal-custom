//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

type selectorCount struct {
	Success   uint64 `json:"success"`
	Failure   uint64 `json:"failure"`
	LastError int32  `json:"last_error"`
	_         uint32
}

type selector struct {
	ports   *ebpf.Map
	counts  *ebpf.Map
	host    *ebpf.Map
	program *ebpf.Program
	link    link.Link
}

func newSelector() (_ *selector, err error) {
	if len(algorithmName) == 0 || len(algorithmName) > 15 {
		return nil, errors.New("invalid congestion algorithm name")
	}
	s := &selector{}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	s.ports, err = ebpf.NewMap(&ebpf.MapSpec{Name: "brutal_ports", Type: ebpf.Hash, KeySize: 4, ValueSize: 1, MaxEntries: 1024})
	if err != nil {
		return nil, fmt.Errorf("port map: %w", err)
	}
	s.host, err = ebpf.NewMap(&ebpf.MapSpec{Name: "brutal_netns", Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		return nil, fmt.Errorf("netns map: %w", err)
	}
	s.counts, err = ebpf.NewMap(&ebpf.MapSpec{Name: "brutal_counts", Type: ebpf.Array, KeySize: 4, ValueSize: 24, MaxEntries: 65536})
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
		Name: "brutal_port_select", Type: ebpf.SockOps, AttachType: ebpf.AttachCGroupSockOps,
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
	var name [16]byte
	copy(name[:], algorithmName)
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
		asm.LoadMem(asm.R0, asm.R0, 0, asm.Byte),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.StoreImm(asm.RFP, -32, int64(binary.LittleEndian.Uint32(name[0:4])), asm.Word),
		asm.StoreImm(asm.RFP, -28, int64(binary.LittleEndian.Uint32(name[4:8])), asm.Word),
		asm.StoreImm(asm.RFP, -24, int64(binary.LittleEndian.Uint32(name[8:12])), asm.Word),
		asm.StoreImm(asm.RFP, -20, int64(binary.LittleEndian.Uint32(name[12:16])), asm.Word),
		asm.Mov.Reg(asm.R1, asm.R6),
		asm.Mov.Imm(asm.R2, 6),  // IPPROTO_TCP
		asm.Mov.Imm(asm.R3, 13), // TCP_CONGESTION
		asm.Mov.Reg(asm.R4, asm.RFP), asm.Add.Imm(asm.R4, -32),
		asm.Mov.Imm(asm.R5, int32(len(algorithmName)+1)),
		asm.FnSetsockopt.Call(),
		asm.Mov.Reg(asm.R9, asm.R0),
		asm.LoadMapPtr(asm.R1, s.counts.FD()),
		asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -8),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "exit"),
		asm.Mov.Reg(asm.R8, asm.R0),
		asm.Mov.Imm(asm.R1, 1),
		asm.JNE.Imm(asm.R9, 0, "failed"),
		asm.StoreXAdd(asm.R8, asm.R1, asm.DWord),
		asm.Ja.Label("exit"),
		asm.StoreMem(asm.R8, 16, asm.R9, asm.Word).WithSymbol("failed"),
		asm.Add.Imm(asm.R8, 8),
		asm.StoreXAdd(asm.R8, asm.R1, asm.DWord),
		asm.Mov.Imm(asm.R0, 1).WithSymbol("exit"),
		asm.Return(),
	}
}

func (s *selector) Enable(port uint16) error {
	key, one := uint32(port), uint8(1)
	return s.ports.Put(&key, &one)
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

func (s *selector) Close() (result error) {
	if s.link != nil {
		result = errors.Join(result, s.link.Close())
	}
	if s.program != nil {
		result = errors.Join(result, s.program.Close())
	}
	if s.ports != nil {
		result = errors.Join(result, s.ports.Close())
	}
	if s.host != nil {
		result = errors.Join(result, s.host.Close())
	}
	if s.counts != nil {
		result = errors.Join(result, s.counts.Close())
	}
	return result
}

func probeSelector() error {
	for _, network := range []string{"tcp4", "tcp6"} {
		if err := probeNetwork(network); err != nil {
			return fmt.Errorf("%s self-check: %w", network, err)
		}
	}
	fmt.Println("algorithm, group parameters and loopback transfer verified; this is not a throughput benchmark")
	return nil
}

func checkSelectorCount(count selectorCount) error {
	if count.Success == 0 || count.Failure != 0 {
		return fmt.Errorf("algorithm selection failed: selected=%d failed=%d errno=%d", count.Success, count.Failure, count.LastError)
	}
	return nil
}

func probeNetwork(network string) (result error) {
	s, err := newSelector()
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, s.Close()) }()
	address := "127.0.0.1:0"
	if network == "tcp6" {
		address = "[::1]:0"
	}
	states, err := parsePorts()
	if err != nil {
		return fmt.Errorf("port rules: %w", err)
	}
	var listener *net.TCPListener
	for attempts := 0; attempts < 32; attempts++ {
		candidate, e := net.Listen(network, address)
		if e != nil {
			return fmt.Errorf("listener: %w", e)
		}
		port := uint16(candidate.Addr().(*net.TCPAddr).Port)
		used := false
		for _, p := range states {
			used = used || p.Port == port
		}
		if !used {
			listener = candidate.(*net.TCPListener)
			break
		}
		candidate.Close()
	}
	if listener == nil {
		return errors.New("could not allocate an unconfigured test port")
	}
	err = listener.SetDeadline(time.Now().Add(3 * time.Second))
	if err != nil {
		listener.Close()
		return err
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if err = writePort(fmt.Sprintf("add %d rate=625000 gain=20", port)); err != nil {
		return fmt.Errorf("temporary rule: %w", err)
	}
	defer func() {
		if e := writePort(fmt.Sprintf("del %d", port)); e != nil {
			result = errors.Join(result, fmt.Errorf("remove temporary rule: %w", e))
		}
	}()
	states, err = parsePorts()
	if err != nil {
		return fmt.Errorf("read temporary group: %w", err)
	}
	var group uint64
	for _, state := range states {
		if state.Port == port && state.Active {
			group = state.Group
		}
	}
	if group == 0 {
		return errors.New("temporary group not visible")
	}
	if err = s.Enable(port); err != nil {
		return fmt.Errorf("enable temporary selector: %w", err)
	}
	defer func() { result = errors.Join(result, s.Disable(port)) }()
	client, err := net.DialTimeout(network, listener.Addr().String(), 3*time.Second)
	if err != nil {
		return err
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		return err
	}
	defer server.Close()
	if err = client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	if err = server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	if err = checkProbeSocket(server.(*net.TCPConn), group); err != nil {
		return err
	}
	payload := make([]byte, 4096)
	if _, err = client.Write(payload); err != nil {
		return fmt.Errorf("client send: %w", err)
	}
	if _, err = io.ReadFull(server, payload); err != nil {
		return fmt.Errorf("server receive: %w", err)
	}
	if _, err = server.Write(payload); err != nil {
		return fmt.Errorf("server send: %w", err)
	}
	if _, err = io.ReadFull(client, payload); err != nil {
		return fmt.Errorf("client receive: %w", err)
	}
	count, err := s.Count(port)
	if err != nil {
		return err
	}
	return checkSelectorCount(count)
}

func checkProbeSocket(c *net.TCPConn, group uint64) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var checkErr error
	err = raw.Control(func(fd uintptr) {
		name, e := unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
		if e != nil {
			checkErr = e
			return
		}
		if name != algorithmName {
			checkErr = fmt.Errorf("actual algorithm %q, expected %q", name, algorithmName)
			return
		}
		var params [20]byte
		length := uint32(len(params))
		_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, fd, unix.IPPROTO_TCP, 23301, uintptr(unsafe.Pointer(&params[0])), uintptr(unsafe.Pointer(&length)), 0)
		if errno != 0 {
			checkErr = errno
			return
		}
		if length != 20 || binary.NativeEndian.Uint64(params[:8]) != 625000 || binary.NativeEndian.Uint32(params[8:12]) != 20 || binary.NativeEndian.Uint64(params[12:]) != group {
			checkErr = errors.New("actual group parameters do not match temporary rule")
		}
	})
	return errors.Join(err, checkErr)
}
