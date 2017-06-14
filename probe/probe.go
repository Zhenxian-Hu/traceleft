package probe

import (
	"bytes"
	"crypto/sha512"
	"fmt"
	"strings"
	"unsafe"

	"github.com/hashicorp/golang-lru"
	"github.com/iovisor/gobpf/bpffs"
	elflib "github.com/iovisor/gobpf/elf"
)

type Probe struct {
	module        *elflib.Module
	handlerCache  *lru.Cache                  // hash -> *Handler
	pidToHandlers map[int]map[string]struct{} // pid -> syscalls handled
}

func evictHandler(key interface{}, value interface{}) {
	if h, ok := value.(*Handler); ok {
		h.Close()
	}
}

type Handler struct {
	module *elflib.Module

	id string

	name string

	fd    int
	fdRet int
}

func sha512hex(d []byte) string {
	return fmt.Sprintf("%x", sha512.Sum512(d))
}

func newHandler(elfBPF []byte) (*Handler, error) {
	rd := bytes.NewReader(elfBPF)
	handlerBPF := elflib.NewModuleFromReader(rd)

	// perf map is initialized and polled from global object
	elfSectionParams := map[string]elflib.SectionParams{
		"maps/events": {
			SkipPerfMapInitialization: true,
		},
	}

	if err := handlerBPF.Load(elfSectionParams); err != nil {
		return nil, fmt.Errorf("error loading handler: %v", err)
	}

	var fd, fdRet int
	var progArrayName, progArrayNameRet string
	for kp := range handlerBPF.IterKprobes() {
		if strings.HasPrefix(kp.Name, "kprobe/") {
			fd = kp.Fd()
			progArrayName = fmt.Sprintf("%s_progs", strings.TrimPrefix(kp.Name, "kprobe/"))
		} else if strings.HasPrefix(kp.Name, "kretprobe/") {
			fdRet = kp.Fd()
			progArrayNameRet = fmt.Sprintf("%s_progs_ret", strings.TrimPrefix(kp.Name, "kretprobe/"))
		}
	}

	if progArrayName == "" || progArrayNameRet == "" {
		return nil, fmt.Errorf("malformed ELF file, it should contain both a kprobe and a kretprobe")
	}

	name := strings.TrimSuffix(progArrayName, "_progs")
	nameRet := strings.TrimSuffix(progArrayNameRet, "_progs_ret")
	if strings.Compare(name, nameRet) != 0 {
		return nil, fmt.Errorf("malformed ELF file, both kprobe and kretprobe should have the same name")
	}

	return &Handler{
		module: handlerBPF,
		name:   name,
		fd:     fd,
		fdRet:  fdRet,
	}, nil
}

func generateProgArrayNames(name string) (progArrayName string, progArrayNameRet string) {
	progArrayName = fmt.Sprintf("%s_progs", name)
	progArrayNameRet = fmt.Sprintf("%s_progs_ret", name)
	return
}

func (probe *Probe) registerHandler(pid int, handler *Handler) error {
	progArrayName, progArrayNameRet := generateProgArrayNames(handler.name)

	progTable := probe.module.Map(progArrayName)
	if progTable == nil {
		return fmt.Errorf("%q doesn't exist", progArrayName)
	}
	progTableRet := probe.module.Map(progArrayNameRet)
	if progTableRet == nil {
		return fmt.Errorf("%q doesn't exist", progArrayNameRet)
	}

	var fd, fdRet int = handler.fd, handler.fdRet
	if err := probe.module.UpdateElement(progTable, unsafe.Pointer(&pid), unsafe.Pointer(&fd), 0); err != nil {
		return fmt.Errorf("error updating %q: %v", progTable.Name, err)
	}
	if err := probe.module.UpdateElement(progTableRet, unsafe.Pointer(&pid), unsafe.Pointer(&fdRet), 0); err != nil {
		return fmt.Errorf("error updating %q: %v", progTableRet.Name, err)
	}

	if _, ok := probe.pidToHandlers[pid]; !ok {
		probe.pidToHandlers[pid] = make(map[string]struct{})
	}

	probe.pidToHandlers[pid][handler.name] = struct{}{}

	return nil
}

func (probe *Probe) RegisterHandlerById(pid int, hash string) error {
	val, ok := probe.handlerCache.Get(hash)
	if !ok {
		return fmt.Errorf("ELF object with hash %q not in the cache", hash)
	}

	handler, ok := val.(*Handler)
	if !ok {
		return fmt.Errorf("invalid type")
	}

	return probe.registerHandler(pid, handler)
}

func (probe *Probe) getHandler(elfBPF []byte) (handler *Handler, err error) {
	id := sha512hex(elfBPF)
	val, ok := probe.handlerCache.Get(id)
	if !ok {
		handler, err = newHandler(elfBPF)
		if err != nil {
			return
		}

		probe.handlerCache.Add(id, handler)

		return
	}

	handler, ok = val.(*Handler)
	if !ok {
		return nil, fmt.Errorf("invalid type")
	}

	return
}

func (probe *Probe) RegisterHandler(pid int, elfBPF []byte) error {
	handler, err := probe.getHandler(elfBPF)
	if err != nil {
		return err
	}

	return probe.registerHandler(pid, handler)
}

func (probe *Probe) unregisterHandler(pid int, handlerName string) error {
	progArrayName, progArrayNameRet := generateProgArrayNames(handlerName)
	progTable := probe.module.Map(progArrayName)
	if progTable == nil {
		return fmt.Errorf("%q doesn't exist", progArrayName)
	}
	progTableRet := probe.module.Map(progArrayNameRet)
	if progTableRet == nil {
		return fmt.Errorf("%q doesn't exist", progArrayNameRet)
	}

	if err := probe.module.DeleteElement(progTable, unsafe.Pointer(&pid)); err != nil {
		return fmt.Errorf("error deleting %q: %v", progTable.Name, err)
	}
	if err := probe.module.DeleteElement(progTableRet, unsafe.Pointer(&pid)); err != nil {
		return fmt.Errorf("error deleting %q: %v", progTableRet.Name, err)
	}
	delete(probe.pidToHandlers, pid)

	return nil
}

func (probe *Probe) UnregisterHandler(pid int) error {
	for handlerName, _ := range probe.pidToHandlers[pid] {
		if err := probe.unregisterHandler(pid, handlerName); err != nil {
			return err
		}
	}

	return nil
}

func (probe *Probe) Close() error {
	return probe.module.Close()
}

func (probe *Probe) BPFModule() *elflib.Module {
	return probe.module
}

func (handler *Handler) Id() string {
	return handler.id
}

func (handler *Handler) Close() error {
	return handler.module.Close()
}

func New(cacheSize int) (*Probe, error) {
	if err := bpffs.Mount(); err != nil {
		return nil, err
	}
	// FIXME move this to go-bindata?
	globalBPF := elflib.NewModule("./bpf/out/trace_syscalls.bpf")

	if err := globalBPF.Load(nil); err != nil {
		return nil, fmt.Errorf("error loading global BPF: %v", err)
	}

	// TODO choose something here
	if err := globalBPF.EnableKprobes(16); err != nil {
		return nil, err
	}

	cache, err := lru.NewWithEvict(cacheSize, evictHandler)
	if err != nil {
		return nil, err
	}

	return &Probe{
		module:        globalBPF,
		handlerCache:  cache,
		pidToHandlers: make(map[int]map[string]struct{}),
	}, nil
}
