// Package wayland provides partial bindings for libwayland.

// Only the subset of client API needed for Gutter has been bound. No thought has been
// given to code generation or supporting arbitrary, user-supplied protocol extensions.
package wayland

// #cgo pkg-config: wayland-client wayland-egl
// #include <stdlib.h>
// #include <wayland-client.h>
// #include "xdg-shell-client-protocol.h"
// #include "xdg-decoration-client-protocol.h"
// #include "wp-presentation-time-client-protocol.h"
// #include "wp-viewporter-client-protocol.h"
//
// int dispatcher(void *user_data, void *target, uint32_t opcode, struct wl_message *msg, union wl_argument *args);
import "C"

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"unicode"
	"unsafe"

	"honnef.co/go/safeish"
)

//go:generate ./generate_wayland.sh

// XXX check if all the creation functions can return errors

var CompositorInterface = &C.wl_compositor_interface
var ShmInterface = &C.wl_shm_interface
var XdgWmBaseInterface = &C.xdg_wm_base_interface
var ZxdgDecorationManagerV1Interface = &C.zxdg_decoration_manager_v1_interface
var WpPresentationInterface = &C.wp_presentation_interface
var WpViewporterInterface = &C.wp_viewporter_interface

type Display struct {
	hnd     *C.struct_wl_display
	proxies map[*C.struct_wl_proxy]any
	pinner  runtime.Pinner

	methods map[methodKey]reflect.Method
	// space reused by dispatcher for creating call args
	callArgs []reflect.Value
	// space reused by dispatcher for computing method name
	methName []byte
}

type methodKey struct {
	typ  reflect.Type
	name string
}

func Connect() (*Display, error) {
	dsp, err := C.wl_display_connect(nil)
	if dsp == nil {
		return nil, fmt.Errorf("couldn't connect to Wayland server: %s", err)
	}
	d := &Display{
		hnd:     dsp,
		proxies: make(map[*C.struct_wl_proxy]any),
		methods: make(map[methodKey]reflect.Method),
	}
	d.pinner.Pin(d)
	return d, nil
}

func (dsp *Display) Handle() unsafe.Pointer {
	return unsafe.Pointer(dsp.hnd)
}

func (dsp *Display) Disconnect() {
	if dsp.hnd == nil {
		panic("double close of wayland.Display")
	}
	C.wl_display_disconnect(dsp.hnd)
	dsp.hnd = nil
	dsp.pinner.Unpin()
}

func (dsp *Display) Fd() uintptr {
	return uintptr(C.wl_display_get_fd(dsp.hnd))
}

func (dsp *Display) Flush() (int, error) {
	n, err := C.wl_display_flush(dsp.hnd)
	return int(n), err
}

func (dsp *Display) PrepareRead() int {
	return int(C.wl_display_prepare_read(dsp.hnd))
}

func (dsp *Display) ReadEvents() error {
	n, err := C.wl_display_read_events(dsp.hnd)
	if n != 0 && err == nil {
		return errors.New("unexpected error in ReadEvents")
	}
	return err
}

func (dsp *Display) CancelRead() {
	C.wl_display_cancel_read(dsp.hnd)
}

func (dsp *Display) DispatchPending() int {
	n := int(C.wl_display_dispatch_pending(dsp.hnd))
	return n
}

func (dsp *Display) Dispatch() int {
	n := int(C.wl_display_dispatch(dsp.hnd))
	return n
}

func (dsp *Display) Roundtrip() (int, error) {
	n, err := C.wl_display_roundtrip(dsp.hnd)
	return int(n), err
}

func (dsp *Display) Registry() *Registry {
	reg := &Registry{
		dsp: dsp,
		hnd: C.wl_display_get_registry(dsp.hnd),
	}
	dsp.add((*C.struct_wl_proxy)(reg.hnd), reg)
	return reg
}

func (dsp *Display) add(proxy *C.struct_wl_proxy, obj any) {
	dsp.proxies[proxy] = obj
	dsp.addDispatcher(proxy)
}

func (dsp *Display) addDispatcher(proxy *C.struct_wl_proxy) {
	C.wl_proxy_add_dispatcher(proxy, (*[0]byte)(C.dispatcher), unsafe.Pointer(&dsp.hnd), nil)
}

func (dsp *Display) forget(proxy *C.struct_wl_proxy) {
	delete(dsp.proxies, proxy)
}

type Callback struct {
	dsp    *Display
	hnd    *C.struct_wl_callback
	OnDone func(data uint32)
}

func (cb *Callback) internal() any {
	return (*callback)(cb)
}

func (cb *Callback) Destroy() {
	C.wl_callback_destroy(cb.hnd)
	cb.dsp.forget((*C.struct_wl_proxy)(cb.hnd))
	cb.hnd = nil
}

type callback Callback

func (cb *callback) Done(data uint32) {
	(cb).OnDone(data)
	(*Callback)(cb).Destroy()
}

func (dsp *Display) Sync(fn func(data uint32)) {
	cb := &Callback{
		dsp:    dsp,
		hnd:    C.wl_display_sync(dsp.hnd),
		OnDone: fn,
	}
	dsp.add((*C.struct_wl_proxy)(cb.hnd), cb)
}

type Output uint32

//export dispatcher
func dispatcher(
	// XXX find out what this function is meant to return
	data unsafe.Pointer,
	target unsafe.Pointer,
	opcode uint32,
	msg *C.struct_wl_message,
	args *C.union_wl_argument,
) C.int {
	dsp := (*Display)(data)
	sig := C.GoString(msg.signature)
	obj := dsp.proxies[(*C.struct_wl_proxy)(target)]
	if obj == nil {
		// XXX don't panic
		panic("don't know this proxy")
	}

	n := safeish.FindNull(safeish.Cast[*byte](msg.name))
	methNameB := dsp.methName
	if cap(methNameB) >= n {
		methNameB = methNameB[:n]
	} else {
		methNameB = make([]byte, n)
		dsp.methName = methNameB[:0]
	}
	copy(methNameB, unsafe.Slice(safeish.Cast[*byte](msg.name), n))
	// Wayland doesn't use Unicode in event names, so this is fine.
	methNameB[0] = byte(unicode.ToUpper(rune(methNameB[0])))
	methName := unsafe.String(&methNameB[0], len(methNameB))

	// XXX validate arg length, and function name
	var meth reflect.Value
	var recv reflect.Value
	if inter, ok := obj.(internaler); ok {
		internal := inter.internal()
		typ := reflect.TypeOf(internal)
		tmeth, ok := dsp.methods[methodKey{typ: typ, name: methName}]
		if !ok {
			tmeth, ok = typ.MethodByName(methName)
			if !ok {
				// XXX don't panic
				panic(fmt.Sprintf("couldn't find method %q on %T", methNameB, inter.internal()))
			}
			dsp.methods[methodKey{typ: typ, name: strings.Clone(methName)}] = tmeth
		}
		meth = tmeth.Func
		recv = reflect.ValueOf(internal)
	} else {
		meth = reflect.ValueOf(obj).Elem().FieldByName("On" + methName)
		if !meth.IsValid() {
			// XXX don't panic
			panic(fmt.Sprintf("couldn't find field %q on %T", "On"+methName, obj))
		}
	}
	if meth.IsNil() {
		// panic(fmt.Sprintln("no callback for", methName))
		return 0
	}

	var i int
	var argOffset int
	callArgs := dsp.callArgs[:0]
	if recv.IsValid() {
		i++
		argOffset = -1
		callArgs = append(callArgs, recv)
	}
	for _, c := range sig {
		arg := unsafe.Add(unsafe.Pointer(args), (i+argOffset)*len(C.union_wl_argument{}))
		// XXX validate that i < meth.Type().NumIn
		// XXX validate that types match
		switch c {
		case 'i':
			callArgs = append(callArgs, reflect.ValueOf(*(*int32)(arg)).Convert(meth.Type().In(int(i))))
		case 'u':
			callArgs = append(callArgs, reflect.ValueOf(*(*uint32)(arg)).Convert(meth.Type().In(int(i))))
		case 'f':
			callArgs = append(callArgs, reflect.ValueOf(*(*C.wl_fixed_t)(arg)))
		case 's':
			callArgs = append(callArgs, reflect.ValueOf(C.GoString(*(**C.char)(arg))))
		case 'o':
			callArgs = append(callArgs, reflect.ValueOf(*(*uint32)(arg)).Convert(meth.Type().In(int(i))))
		case 'n':
			panic("n")
			// XXX
		case 'a':
			arr := *(**C.struct_wl_array)(arg)
			// XXX make sure that calling Elem won't panic
			// XXX validate that arr.Size and arr.Alloc make sense for the given element type
			switch elem := meth.Type().In(int(i)).Elem(); elem {
			case reflect.TypeOf(int32(0)):
				callArgs = append(callArgs, reflect.ValueOf(unsafe.Slice((*int32)(arr.data), arr.size/4)))
			case reflect.TypeOf(uint32(0)):
				callArgs = append(callArgs, reflect.ValueOf(unsafe.Slice((*uint32)(arr.data), arr.size/4)))
			default:
				// XXX support all types we need
				// XXX support convertible types
				panic(fmt.Sprintf("unsupported array element type %s", elem))
			}

		case 'h':
			panic("h")
			// XXX
		case '?':
			continue
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			continue
		default:
			panic(c)
		}
		i++
	}
	if !meth.IsNil() {
		meth.Call(callArgs)
	}
	dsp.callArgs = callArgs[:0]
	return 0
}

type Registry struct {
	dsp *Display
	hnd *C.struct_wl_registry

	OnGlobal       func(name uint32, iface string, version uint32)
	OnGlobalRemove func(name uint32)
}

type internaler interface {
	internal() any
}

func (reg *Registry) Destroy() {
	C.wl_registry_destroy(reg.hnd)
	reg.dsp.forget((*C.struct_wl_proxy)(reg.hnd))
	reg.hnd = nil
}

func (reg *Registry) bind(name uint32, iface *C.struct_wl_interface, vers uint32) *C.struct_wl_proxy {
	return (*C.struct_wl_proxy)(C.wl_registry_bind(reg.hnd, C.uint(name), iface, C.uint(vers)))
}

func (reg *Registry) BindCompositor(name uint32, vers uint32) *Compositor {
	comp := &Compositor{
		dsp:  reg.dsp,
		hnd:  (*C.struct_wl_compositor)(reg.bind(name, CompositorInterface, vers)),
		vers: int(vers),
	}
	reg.dsp.add((*C.struct_wl_proxy)(comp.hnd), comp)
	return comp
}

func (reg *Registry) BindShm(name uint32, vers uint32) *Shm {
	shm := &Shm{
		dsp:  reg.dsp,
		hnd:  (*C.struct_wl_shm)(reg.bind(name, ShmInterface, vers)),
		vers: int(vers),
	}
	reg.dsp.add((*C.struct_wl_proxy)(shm.hnd), shm)
	return shm
}

func (reg *Registry) BindXdgWmBase(name uint32, vers uint32) *XdgWmBase {
	xdg := &XdgWmBase{
		dsp:  reg.dsp,
		hnd:  (*C.struct_xdg_wm_base)(reg.bind(name, XdgWmBaseInterface, vers)),
		vers: int(vers),
	}
	reg.dsp.add((*C.struct_wl_proxy)(xdg.hnd), xdg)
	return xdg
}

func (reg *Registry) BindZxdgDecorationManagerV1(name uint32, vers uint32) *XdgDecorationManager {
	xdg := &XdgDecorationManager{
		dsp:  reg.dsp,
		hnd:  (*C.struct_zxdg_decoration_manager_v1)(reg.bind(name, ZxdgDecorationManagerV1Interface, vers)),
		vers: int(vers),
	}
	reg.dsp.add((*C.struct_wl_proxy)(xdg.hnd), xdg)
	return xdg
}

func (reg *Registry) BindWpPresentation(name uint32, vers uint32) *WpPresentation {
	out := &WpPresentation{
		dsp:  reg.dsp,
		hnd:  (*C.struct_wp_presentation)(reg.bind(name, WpPresentationInterface, vers)),
		vers: int(vers),
	}
	reg.dsp.add((*C.struct_wl_proxy)(out.hnd), out)
	return out
}

func (reg *Registry) BindWpViewporter(name uint32, vers uint32) *WpViewporter {
	out := &WpViewporter{
		dsp:  reg.dsp,
		hnd:  (*C.struct_wp_viewporter)(reg.bind(name, WpViewporterInterface, vers)),
		vers: int(vers),
	}
	reg.dsp.add((*C.struct_wl_proxy)(out.hnd), out)
	return out
}

type WpPresentation struct {
	dsp        *Display
	hnd        *C.struct_wp_presentation
	vers       int
	OnClock_id func(id uint)
}

func (p *WpPresentation) Version() int { return p.vers }

func (p *WpPresentation) Feedback(surface *Surface) *WpPresentationFeedback {
	out := &WpPresentationFeedback{
		dsp:  p.dsp,
		hnd:  C.wp_presentation_feedback(p.hnd, surface.hnd),
		vers: p.vers,
	}
	p.dsp.add((*C.struct_wl_proxy)(out.hnd), out)
	return out
}

func (p *WpPresentation) Destroy() {
	C.wp_presentation_destroy(p.hnd)
	p.dsp.forget((*C.struct_wl_proxy)(p.hnd))
}

type WpPresentationFeedback struct {
	dsp          *Display
	hnd          *C.struct_wp_presentation_feedback
	vers         int
	OnSyncOutput func(*Output)
	OnPresented  func(
		tvSecHi, tvSecLo, tvNsec uint32,
		refresh uint32,
		seqHi, seqLo uint32,
		flags uint32,
	)
	OnDiscarded func()
}

func (p *WpPresentationFeedback) Version() int { return p.vers }

func (p *WpPresentationFeedback) internal() any {
	return (*wpPresentationFeedback)(p)
}

type wpPresentationFeedback WpPresentationFeedback

func (p *wpPresentationFeedback) SyncOutput(out *Output) {
	p.OnSyncOutput(out)
}

func (p *wpPresentationFeedback) Presented(
	tvSecHi, tvSecLo, tvNsec uint32,
	refresh uint32,
	seqHi, seqLo uint32,
	flags uint32,
) {
	p.OnPresented(
		tvSecHi, tvSecLo, tvNsec,
		refresh,
		seqHi, seqLo,
		flags,
	)
	p.dsp.forget((*C.struct_wl_proxy)(p.hnd))
}

func (p *wpPresentationFeedback) Discarded() {
	p.OnDiscarded()
	p.dsp.forget((*C.struct_wl_proxy)(p.hnd))
}

type Compositor struct {
	dsp  *Display
	hnd  *C.struct_wl_compositor
	vers int
}

func (comp *Compositor) Version() int { return comp.vers }

func (comp *Compositor) CreateSurface() *Surface {
	surf := &Surface{
		dsp:  comp.dsp,
		hnd:  C.wl_compositor_create_surface(comp.hnd),
		vers: comp.vers,
	}
	comp.dsp.add((*C.struct_wl_proxy)(surf.hnd), surf)
	return surf
}

func (comp *Compositor) Destroy() {
	C.wl_compositor_destroy(comp.hnd)
	comp.dsp.forget((*C.struct_wl_proxy)(comp.hnd))
}

type Surface struct {
	dsp  *Display
	hnd  *C.struct_wl_surface
	vers int

	OnPreferred_buffer_scale func(scale int)
}

func (surf *Surface) Version() int { return surf.vers }

func (surf *Surface) Handle() unsafe.Pointer {
	return unsafe.Pointer(surf.hnd)
}

func (surf *Surface) Destroy() {
	C.wl_surface_destroy(surf.hnd)
	surf.dsp.forget((*C.struct_wl_proxy)(surf.hnd))
}

func (surf *Surface) Attach(buf *Buffer) {
	C.wl_surface_attach(surf.hnd, buf.hnd, 0, 0)
}

func (surf *Surface) SetBufferScale(scale int) {
	C.wl_surface_set_buffer_scale(surf.hnd, C.int32_t(scale))
}

func (surf *Surface) Damage(x, y, width, height int32) {
	C.wl_surface_damage(surf.hnd, C.int(x), C.int(y), C.int(width), C.int(height))
}

func (surf *Surface) Frame(fn func(data uint32)) {
	cb := &Callback{
		dsp:    surf.dsp,
		hnd:    C.wl_surface_frame(surf.hnd),
		OnDone: fn,
	}
	surf.dsp.add((*C.struct_wl_proxy)(cb.hnd), cb)
}

func (surf *Surface) Commit() {
	C.wl_surface_commit(surf.hnd)
}

type Shm struct {
	dsp  *Display
	hnd  *C.struct_wl_shm
	vers int
	// XXX format should be of type SHmFormat, but for that we have to improve our
	// reflection.
	OnFormat func(format uint32)
}

func (shm *Shm) Version() int { return shm.vers }

func (shm *Shm) Destroy() {
	C.wl_shm_destroy(shm.hnd)
	shm.dsp.forget((*C.struct_wl_proxy)(shm.hnd))
}

func (shm *Shm) CreatePool(fd int32, sz int32) *ShmPool {
	pool := &ShmPool{
		dsp:  shm.dsp,
		hnd:  C.wl_shm_create_pool(shm.hnd, C.int(fd), C.int(sz)),
		vers: shm.vers,
	}
	shm.dsp.add((*C.struct_wl_proxy)(pool.hnd), pool)
	return pool
}

type ShmPool struct {
	dsp  *Display
	hnd  *C.struct_wl_shm_pool
	vers int
}

func (pool *ShmPool) Version() int { return pool.vers }

func (pool *ShmPool) Destroy() {
	C.wl_shm_pool_destroy(pool.hnd)
	pool.dsp.forget((*C.struct_wl_proxy)(pool.hnd))
}

func (pool *ShmPool) CreateBuffer(offset, width, height, stride int32, format ShmFormat) *Buffer {
	buf := &Buffer{
		dsp:  pool.dsp,
		hnd:  C.wl_shm_pool_create_buffer(pool.hnd, C.int(offset), C.int(width), C.int(height), C.int(stride), C.uint(format)),
		vers: pool.vers,
	}
	pool.dsp.add((*C.struct_wl_proxy)(buf.hnd), buf)
	return buf
}

//go:generate stringer -type ShmFormat
type ShmFormat uint32

const (
	ShmFormatArgb8888             ShmFormat = 0          // 32-bit ARGB format, [31:0] A:R:G:B 8:8:8:8 little endian
	ShmFormatXrgb8888             ShmFormat = 1          // 32-bit RGB format, [31:0] x:R:G:B 8:8:8:8 little endian
	ShmFormatC8                   ShmFormat = 0x20203843 // 8-bit color index format, [7:0] C
	ShmFormatRgb332               ShmFormat = 0x38424752 // 8-bit RGB format, [7:0] R:G:B 3:3:2
	ShmFormatBgr233               ShmFormat = 0x38524742 // 8-bit BGR format, [7:0] B:G:R 2:3:3
	ShmFormatXrgb4444             ShmFormat = 0x32315258 // 16-bit xRGB format, [15:0] x:R:G:B 4:4:4:4 little endian
	ShmFormatXbgr4444             ShmFormat = 0x32314258 // 16-bit xBGR format, [15:0] x:B:G:R 4:4:4:4 little endian
	ShmFormatRgbx4444             ShmFormat = 0x32315852 // 16-bit RGBx format, [15:0] R:G:B:x 4:4:4:4 little endian
	ShmFormatBgrx4444             ShmFormat = 0x32315842 // 16-bit BGRx format, [15:0] B:G:R:x 4:4:4:4 little endian
	ShmFormatArgb4444             ShmFormat = 0x32315241 // 16-bit ARGB format, [15:0] A:R:G:B 4:4:4:4 little endian
	ShmFormatAbgr4444             ShmFormat = 0x32314241 // 16-bit ABGR format, [15:0] A:B:G:R 4:4:4:4 little endian
	ShmFormatRgba4444             ShmFormat = 0x32314152 // 16-bit RBGA format, [15:0] R:G:B:A 4:4:4:4 little endian
	ShmFormatBgra4444             ShmFormat = 0x32314142 // 16-bit BGRA format, [15:0] B:G:R:A 4:4:4:4 little endian
	ShmFormatXrgb1555             ShmFormat = 0x35315258 // 16-bit xRGB format, [15:0] x:R:G:B 1:5:5:5 little endian
	ShmFormatXbgr1555             ShmFormat = 0x35314258 // 16-bit xBGR 1555 format, [15:0] x:B:G:R 1:5:5:5 little endian
	ShmFormatRgbx5551             ShmFormat = 0x35315852 // 16-bit RGBx 5551 format, [15:0] R:G:B:x 5:5:5:1 little endian
	ShmFormatBgrx5551             ShmFormat = 0x35315842 // 16-bit BGRx 5551 format, [15:0] B:G:R:x 5:5:5:1 little endian
	ShmFormatArgb1555             ShmFormat = 0x35315241 // 16-bit ARGB 1555 format, [15:0] A:R:G:B 1:5:5:5 little endian
	ShmFormatAbgr1555             ShmFormat = 0x35314241 // 16-bit ABGR 1555 format, [15:0] A:B:G:R 1:5:5:5 little endian
	ShmFormatRgba5551             ShmFormat = 0x35314152 // 16-bit RGBA 5551 format, [15:0] R:G:B:A 5:5:5:1 little endian
	ShmFormatBgra5551             ShmFormat = 0x35314142 // 16-bit BGRA 5551 format, [15:0] B:G:R:A 5:5:5:1 little endian
	ShmFormatRgb565               ShmFormat = 0x36314752 // 16-bit RGB 565 format, [15:0] R:G:B 5:6:5 little endian
	ShmFormatBgr565               ShmFormat = 0x36314742 // 16-bit BGR 565 format, [15:0] B:G:R 5:6:5 little endian
	ShmFormatRgb888               ShmFormat = 0x34324752 // 24-bit RGB format, [23:0] R:G:B little endian
	ShmFormatBgr888               ShmFormat = 0x34324742 // 24-bit BGR format, [23:0] B:G:R little endian
	ShmFormatXbgr8888             ShmFormat = 0x34324258 // 32-bit xBGR format, [31:0] x:B:G:R 8:8:8:8 little endian
	ShmFormatRgbx8888             ShmFormat = 0x34325852 // 32-bit RGBx format, [31:0] R:G:B:x 8:8:8:8 little endian
	ShmFormatBgrx8888             ShmFormat = 0x34325842 // 32-bit BGRx format, [31:0] B:G:R:x 8:8:8:8 little endian
	ShmFormatAbgr8888             ShmFormat = 0x34324241 // 32-bit ABGR format, [31:0] A:B:G:R 8:8:8:8 little endian
	ShmFormatRgba8888             ShmFormat = 0x34324152 // 32-bit RGBA format, [31:0] R:G:B:A 8:8:8:8 little endian
	ShmFormatBgra8888             ShmFormat = 0x34324142 // 32-bit BGRA format, [31:0] B:G:R:A 8:8:8:8 little endian
	ShmFormatXrgb2101010          ShmFormat = 0x30335258 // 32-bit xRGB format, [31:0] x:R:G:B 2:10:10:10 little endian
	ShmFormatXbgr2101010          ShmFormat = 0x30334258 // 32-bit xBGR format, [31:0] x:B:G:R 2:10:10:10 little endian
	ShmFormatRgbx1010102          ShmFormat = 0x30335852 // 32-bit RGBx format, [31:0] R:G:B:x 10:10:10:2 little endian
	ShmFormatBgrx1010102          ShmFormat = 0x30335842 // 32-bit BGRx format, [31:0] B:G:R:x 10:10:10:2 little endian
	ShmFormatArgb2101010          ShmFormat = 0x30335241 // 32-bit ARGB format, [31:0] A:R:G:B 2:10:10:10 little endian
	ShmFormatAbgr2101010          ShmFormat = 0x30334241 // 32-bit ABGR format, [31:0] A:B:G:R 2:10:10:10 little endian
	ShmFormatRgba1010102          ShmFormat = 0x30334152 // 32-bit RGBA format, [31:0] R:G:B:A 10:10:10:2 little endian
	ShmFormatBgra1010102          ShmFormat = 0x30334142 // 32-bit BGRA format, [31:0] B:G:R:A 10:10:10:2 little endian
	ShmFormatYuyv                 ShmFormat = 0x56595559 // packed YCbCr format, [31:0] Cr0:Y1:Cb0:Y0 8:8:8:8 little endian
	ShmFormatYvyu                 ShmFormat = 0x55595659 // packed YCbCr format, [31:0] Cb0:Y1:Cr0:Y0 8:8:8:8 little endian
	ShmFormatUyvy                 ShmFormat = 0x59565955 // packed YCbCr format, [31:0] Y1:Cr0:Y0:Cb0 8:8:8:8 little endian
	ShmFormatVyuy                 ShmFormat = 0x59555956 // packed YCbCr format, [31:0] Y1:Cb0:Y0:Cr0 8:8:8:8 little endian
	ShmFormatAyuv                 ShmFormat = 0x56555941 // packed AYCbCr format, [31:0] A:Y:Cb:Cr 8:8:8:8 little endian
	ShmFormatNv12                 ShmFormat = 0x3231564e // 2 plane YCbCr Cr:Cb format, 2x2 subsampled Cr:Cb plane
	ShmFormatNv21                 ShmFormat = 0x3132564e // 2 plane YCbCr Cb:Cr format, 2x2 subsampled Cb:Cr plane
	ShmFormatNv16                 ShmFormat = 0x3631564e // 2 plane YCbCr Cr:Cb format, 2x1 subsampled Cr:Cb plane
	ShmFormatNv61                 ShmFormat = 0x3136564e // 2 plane YCbCr Cb:Cr format, 2x1 subsampled Cb:Cr plane
	ShmFormatYuv410               ShmFormat = 0x39565559 // 3 plane YCbCr format, 4x4 subsampled Cb (1) and Cr (2) planes
	ShmFormatYvu410               ShmFormat = 0x39555659 // 3 plane YCbCr format, 4x4 subsampled Cr (1) and Cb (2) planes
	ShmFormatYuv411               ShmFormat = 0x31315559 // 3 plane YCbCr format, 4x1 subsampled Cb (1) and Cr (2) planes
	ShmFormatYvu411               ShmFormat = 0x31315659 // 3 plane YCbCr format, 4x1 subsampled Cr (1) and Cb (2) planes
	ShmFormatYuv420               ShmFormat = 0x32315559 // 3 plane YCbCr format, 2x2 subsampled Cb (1) and Cr (2) planes
	ShmFormatYvu420               ShmFormat = 0x32315659 // 3 plane YCbCr format, 2x2 subsampled Cr (1) and Cb (2) planes
	ShmFormatYuv422               ShmFormat = 0x36315559 // 3 plane YCbCr format, 2x1 subsampled Cb (1) and Cr (2) planes
	ShmFormatYvu422               ShmFormat = 0x36315659 // 3 plane YCbCr format, 2x1 subsampled Cr (1) and Cb (2) planes
	ShmFormatYuv444               ShmFormat = 0x34325559 // 3 plane YCbCr format, non-subsampled Cb (1) and Cr (2) planes
	ShmFormatYvu444               ShmFormat = 0x34325659 // 3 plane YCbCr format, non-subsampled Cr (1) and Cb (2) planes
	ShmFormatR8                   ShmFormat = 0x20203852 // [7:0] R
	ShmFormatR16                  ShmFormat = 0x20363152 // [15:0] R little endian
	ShmFormatRg88                 ShmFormat = 0x38384752 // [15:0] R:G 8:8 little endian
	ShmFormatGr88                 ShmFormat = 0x38385247 // [15:0] G:R 8:8 little endian
	ShmFormatRg1616               ShmFormat = 0x32334752 // [31:0] R:G 16:16 little endian
	ShmFormatGr1616               ShmFormat = 0x32335247 // [31:0] G:R 16:16 little endian
	ShmFormatXrgb16161616f        ShmFormat = 0x48345258 // [63:0] x:R:G:B 16:16:16:16 little endian
	ShmFormatXbgr16161616f        ShmFormat = 0x48344258 // [63:0] x:B:G:R 16:16:16:16 little endian
	ShmFormatArgb16161616f        ShmFormat = 0x48345241 // [63:0] A:R:G:B 16:16:16:16 little endian
	ShmFormatAbgr16161616f        ShmFormat = 0x48344241 // [63:0] A:B:G:R 16:16:16:16 little endian
	ShmFormatXyuv8888             ShmFormat = 0x56555958 // [31:0] X:Y:Cb:Cr 8:8:8:8 little endian
	ShmFormatVuy888               ShmFormat = 0x34325556 // [23:0] Cr:Cb:Y 8:8:8 little endian
	ShmFormatVuy101010            ShmFormat = 0x30335556 // Y followed by U then V, 10:10:10. Non-linear modifier only
	ShmFormatY210                 ShmFormat = 0x30313259 // [63:0] Cr0:0:Y1:0:Cb0:0:Y0:0 10:6:10:6:10:6:10:6 little endian per 2 Y pixels
	ShmFormatY212                 ShmFormat = 0x32313259 // [63:0] Cr0:0:Y1:0:Cb0:0:Y0:0 12:4:12:4:12:4:12:4 little endian per 2 Y pixels
	ShmFormatY216                 ShmFormat = 0x36313259 // [63:0] Cr0:Y1:Cb0:Y0 16:16:16:16 little endian per 2 Y pixels
	ShmFormatY410                 ShmFormat = 0x30313459 // [31:0] A:Cr:Y:Cb 2:10:10:10 little endian
	ShmFormatY412                 ShmFormat = 0x32313459 // [63:0] A:0:Cr:0:Y:0:Cb:0 12:4:12:4:12:4:12:4 little endian
	ShmFormatY416                 ShmFormat = 0x36313459 // [63:0] A:Cr:Y:Cb 16:16:16:16 little endian
	ShmFormatXvyu2101010          ShmFormat = 0x30335658 // [31:0] X:Cr:Y:Cb 2:10:10:10 little endian
	ShmFormatXvyu12_16161616      ShmFormat = 0x36335658 // [63:0] X:0:Cr:0:Y:0:Cb:0 12:4:12:4:12:4:12:4 little endian
	ShmFormatXvyu16161616         ShmFormat = 0x38345658 // [63:0] X:Cr:Y:Cb 16:16:16:16 little endian
	ShmFormatY0l0                 ShmFormat = 0x304c3059 // [63:0] A3:A2:Y3:0:Cr0:0:Y2:0:A1:A0:Y1:0:Cb0:0:Y0:0 1:1:8:2:8:2:8:2:1:1:8:2:8:2:8:2 little endian
	ShmFormatX0l0                 ShmFormat = 0x304c3058 // [63:0] X3:X2:Y3:0:Cr0:0:Y2:0:X1:X0:Y1:0:Cb0:0:Y0:0 1:1:8:2:8:2:8:2:1:1:8:2:8:2:8:2 little endian
	ShmFormatY0l2                 ShmFormat = 0x324c3059 // [63:0] A3:A2:Y3:Cr0:Y2:A1:A0:Y1:Cb0:Y0 1:1:10:10:10:1:1:10:10:10 little endian
	ShmFormatX0l2                 ShmFormat = 0x324c3058 // [63:0] X3:X2:Y3:Cr0:Y2:X1:X0:Y1:Cb0:Y0 1:1:10:10:10:1:1:10:10:10 little endian
	ShmFormatYuv420_8bit          ShmFormat = 0x38305559
	ShmFormatYuv420_10bit         ShmFormat = 0x30315559
	ShmFormatXrgb8888_a8          ShmFormat = 0x38415258
	ShmFormatXbgr8888_a8          ShmFormat = 0x38414258
	ShmFormatRgbx8888_a8          ShmFormat = 0x38415852
	ShmFormatBgrx8888_a8          ShmFormat = 0x38415842
	ShmFormatRgb888_a8            ShmFormat = 0x38413852
	ShmFormatBgr888_a8            ShmFormat = 0x38413842
	ShmFormatRgb565_a8            ShmFormat = 0x38413552
	ShmFormatBgr565_a8            ShmFormat = 0x38413542
	ShmFormatNv24                 ShmFormat = 0x3432564e // non-subsampled Cr:Cb plane
	ShmFormatNv42                 ShmFormat = 0x3234564e // non-subsampled Cb:Cr plane
	ShmFormatP210                 ShmFormat = 0x30313250 // 2x1 subsampled Cr:Cb plane, 10 bit per channel
	ShmFormatP010                 ShmFormat = 0x30313050 // 2x2 subsampled Cr:Cb plane 10 bits per channel
	ShmFormatP012                 ShmFormat = 0x32313050 // 2x2 subsampled Cr:Cb plane 12 bits per channel
	ShmFormatP016                 ShmFormat = 0x36313050 // 2x2 subsampled Cr:Cb plane 16 bits per channel
	ShmFormatAxbxgxrx106106106106 ShmFormat = 0x30314241 // [63:0] A:x:B:x:G:x:R:x 10:6:10:6:10:6:10:6 little endian
	ShmFormatNv15                 ShmFormat = 0x3531564e // 2x2 subsampled Cr:Cb plane
	ShmFormatQ410                 ShmFormat = 0x30313451
	ShmFormatQ401                 ShmFormat = 0x31303451
	ShmFormatXrgb16161616         ShmFormat = 0x38345258 // [63:0] x:R:G:B 16:16:16:16 little endian
	ShmFormatXbgr16161616         ShmFormat = 0x38344258 // [63:0] x:B:G:R 16:16:16:16 little endian
	ShmFormatArgb16161616         ShmFormat = 0x38345241 // [63:0] A:R:G:B 16:16:16:16 little endian
	ShmFormatAbgr16161616         ShmFormat = 0x38344241 // [63:0] A:B:G:R 16:16:16:16 little endian
)

type Buffer struct {
	dsp       *Display
	hnd       *C.struct_wl_buffer
	vers      int
	OnRelease func()
}

func (buf *Buffer) Version() int { return buf.vers }

func (buf *Buffer) Destroy() {
	C.wl_buffer_destroy(buf.hnd)
	buf.dsp.forget((*C.struct_wl_proxy)(buf.hnd))
}

type XdgWmBase struct {
	dsp    *Display
	hnd    *C.struct_xdg_wm_base
	vers   int
	OnPing func(serial uint32)
}

func (xdg *XdgWmBase) Version() int { return xdg.vers }

func (xdg *XdgWmBase) Destroy() {
	C.xdg_wm_base_destroy(xdg.hnd)
	xdg.dsp.forget((*C.struct_wl_proxy)(xdg.hnd))
}

func (xdg *XdgWmBase) XdgSurface(surf *Surface) *XdgSurface {
	xdgSurf := &XdgSurface{
		dsp:  xdg.dsp,
		hnd:  C.xdg_wm_base_get_xdg_surface(xdg.hnd, surf.hnd),
		vers: xdg.vers,
	}
	xdg.dsp.add((*C.struct_wl_proxy)(xdgSurf.hnd), xdgSurf)
	return xdgSurf
}

func (xdg *XdgWmBase) Pong(serial uint32) {
	C.xdg_wm_base_pong(xdg.hnd, C.uint32_t(serial))
}

type XdgSurface struct {
	dsp         *Display
	hnd         *C.struct_xdg_surface
	vers        int
	OnConfigure func(serial uint32)
}

func (surf *XdgSurface) Version() int { return surf.vers }

func (surf *XdgSurface) Destroy() {
	C.xdg_surface_destroy(surf.hnd)
	surf.dsp.forget((*C.struct_wl_proxy)(surf.hnd))
}

func (surf *XdgSurface) Toplevel() *XdgToplevel {
	top := &XdgToplevel{
		dsp:  surf.dsp,
		hnd:  C.xdg_surface_get_toplevel(surf.hnd),
		vers: surf.vers,
	}
	surf.dsp.add((*C.struct_wl_proxy)(top.hnd), top)
	return top
}

func (surf *XdgSurface) AckConfigure(serial uint32) {
	C.xdg_surface_ack_configure(surf.hnd, C.uint(serial))
}

type XdgToplevel struct {
	dsp               *Display
	hnd               *C.struct_xdg_toplevel
	vers              int
	OnConfigure       func(width, height int32, states []uint32)
	OnClose           func()
	OnWm_capabilities func([]uint32)
}

func (top *XdgToplevel) Version() int { return top.vers }

func (top *XdgToplevel) Destroy() {
	C.xdg_toplevel_destroy(top.hnd)
	top.dsp.forget((*C.struct_wl_proxy)(top.hnd))
}

func (top *XdgToplevel) SetTitle(s string) {
	cstr := C.CString(s)
	defer C.free(unsafe.Pointer(cstr))
	C.xdg_toplevel_set_title(top.hnd, cstr)
}

type XdgDecorationManager struct {
	dsp  *Display
	hnd  *C.struct_zxdg_decoration_manager_v1
	vers int
}

func (xdg *XdgDecorationManager) Version() int { return xdg.vers }

func (xdg *XdgDecorationManager) ToplevelDecoration(top *XdgToplevel) *XdgToplevelDecoration {
	dec := &XdgToplevelDecoration{
		dsp:  xdg.dsp,
		hnd:  C.zxdg_decoration_manager_v1_get_toplevel_decoration(xdg.hnd, top.hnd),
		vers: xdg.vers,
	}
	xdg.dsp.add((*C.struct_wl_proxy)(dec.hnd), dec)
	return dec
}

func (xdg *XdgDecorationManager) Destroy() {
	C.zxdg_decoration_manager_v1_destroy(xdg.hnd)
	xdg.dsp.forget((*C.struct_wl_proxy)(xdg.hnd))
}

type XdgToplevelDecoration struct {
	dsp         *Display
	hnd         *C.struct_zxdg_toplevel_decoration_v1
	vers        int
	OnConfigure func(mode XdgToplevelDecorationMode)
}

func (dec *XdgToplevelDecoration) Version() int { return dec.vers }

func (dec *XdgToplevelDecoration) Destroy() {
	C.zxdg_toplevel_decoration_v1_destroy(dec.hnd)
	dec.dsp.forget((*C.struct_wl_proxy)(dec.hnd))
}

func (dec *XdgToplevelDecoration) SetMode(mode XdgToplevelDecorationMode) {
	C.zxdg_toplevel_decoration_v1_set_mode(dec.hnd, C.uint32_t(mode))
}

type WpViewporter struct {
	dsp  *Display
	hnd  *C.struct_wp_viewporter
	vers int
}

func (porter *WpViewporter) Viewport(surf *Surface) *WpViewport {
	out := &WpViewport{
		dsp:  porter.dsp,
		hnd:  C.wp_viewporter_get_viewport(porter.hnd, surf.hnd),
		vers: porter.vers,
	}
	porter.dsp.add((*C.struct_wl_proxy)(out.hnd), out)
	return out
}

func (porter *WpViewporter) Destroy() {
	C.wp_viewporter_destroy(porter.hnd)
	porter.dsp.forget((*C.struct_wl_proxy)(porter.hnd))
}

type WpViewport struct {
	dsp  *Display
	hnd  *C.struct_wp_viewport
	vers int
}

func (port *WpViewport) SetDestination(width, height int) {
	C.wp_viewport_set_destination(port.hnd, C.int32_t(width), C.int32_t(height))
}

func (port *WpViewport) Destroy() {
	C.wp_viewport_destroy(port.hnd)
	port.dsp.forget((*C.struct_wl_proxy)(port.hnd))
}

type XdgToplevelDecorationMode uint32

const (
	XdgToplevelDecorationModeClientSide = C.ZXDG_TOPLEVEL_DECORATION_V1_MODE_CLIENT_SIDE
	XdgToplevelDecorationModeServerSide = C.ZXDG_TOPLEVEL_DECORATION_V1_MODE_SERVER_SIDE
)

const (
	WpPresentationFeedbackKindVsync        = C.WP_PRESENTATION_FEEDBACK_KIND_VSYNC
	WpPresentationFeedbackKindHWClock      = C.WP_PRESENTATION_FEEDBACK_KIND_HW_CLOCK
	WpPresentationFeedbackKindHWCompletion = C.WP_PRESENTATION_FEEDBACK_KIND_HW_COMPLETION
	WpPresentationFeedbackKindZeroCopy     = C.WP_PRESENTATION_FEEDBACK_KIND_ZERO_COPY
)
