//go:build js

package web

import (
	"log"
	"math"
	"strconv"
	"syscall/js"
	"unicode/utf8"

	"github.com/egoist/mygo/internal/gpu/gl"
	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/transfer"
)

// surface is the canvas of a window, drawn with WebGL 2, or in memory and
// shown with a 2D context where the browser has no WebGL 2. A textarea out
// of sight has the keyboard while the window has: input methods compose
// in it, at the caret of the content, and its clipboard events carry the
// clipboard. Its inputmode is "none" while nothing in the content takes
// text, so that touch devices, which show their keyboard for a focused
// textarea, show it only for a text input.
type surface struct {
	w             *window
	canvas, input js.Value
	gl            *gl.Context
	presenter     gl.Presenter
	ctx2d         js.Value
	image         js.Value // ImageData of the frames drawn in memory
	rgba          []byte

	// width and height are the canvas's size in DIPs, CSS pixels, and
	// scale the device pixels of one.
	width, height, scale float64
	pending              bool
	request              js.Value // the requestAnimationFrame pending
	frame                js.Func
	funcs                []js.Func
	global               []listener
	observer             js.Value
	closed               bool
	composing            bool
	text                 platform.TextInputState
	// copied tells that a key copying went to the content, which the copy
	// event that follows does not repeat.
	copied bool

	// downX and downY place the last press, where a context menu opens.
	downX, downY float64
	clicks       platform.ClickCounter

	// native is true on touch devices (a coarse pointer). There
	// the textarea lies over the focused field and holds its text and
	// selection, so the system's own editing works on it: the long-press
	// menu (Paste, Select, Copy), selection handles, autocorrect and
	// predictive text, and input methods. The browser edits the textarea
	// and each change goes to the field's text client as a replacement of
	// what changed (nativeSync). mirror is the text last given to it, and
	// pushed the field's selection last given to it.
	native    bool
	area      js.Value // the textarea
	user      js.Value // a sign-in form's username input (touch devices)
	pass      js.Value // and its password input
	switching bool     // the keyboard moves between them: not a blur
	mirror    string
	pushed    platform.TextRange
	pressedAt float64
	pressX    float64
	pressY    float64

	// touch is the finger on the canvas (touch devices): it scrolls.
	touch touchState
}

// touchState follows a finger on the canvas. A finger that moves before it
// has been held for touchHold scrolls what is under it, and keeps scrolling
// once it lifts, slowing down; a tap is a click; a finger held still
// presses there, as the mouse does, and its moves then drag (selecting
// text). Pointer events tell no more than this: the canvas takes them all
// (touch-action: none), and the browser scrolls nothing.
type touchState struct {
	id                 int     // the pointer followed, 0 with none
	startX, startY     float64 // where it went down
	lastX, lastY, last float64 // where it was last, and when (ms)
	vx, vy             float64 // its speed (DIPs a ms)
	scrolling, pressed bool
	hold               js.Value // the setTimeout of a press
	fling              js.Value // the requestAnimationFrame of a fling
	flingAt            float64
	holdFunc, flingFn  js.Func
}

// touchSlop is how far a finger moves before it scrolls (DIPs), touchHold
// how long it is held still to press (ms), and touchDecay how much of its
// speed a fling keeps each ms, as iOS scroll views.
const (
	touchSlop  = 8
	touchHold  = 450
	touchDecay = 0.998
)

func newSurface(w *window) (*surface, error) {
	doc := js.Global().Get("document")
	s := &surface{w: w, scale: 1}
	s.canvas = doc.Call("createElement", "canvas")
	s.canvas.Set("tabIndex", -1)
	s.input = doc.Call("createElement", "textarea")
	s.native = js.Global().Call("matchMedia", "(pointer: coarse)").Get("matches").Bool()
	attrs := map[string]string{"autocomplete": "off", "autocorrect": "off", "autocapitalize": "off", "spellcheck": "false", "tabindex": "-1", "inputmode": "none"}
	if s.native {
		// the system's corrections and predictions work on the field's own text
		attrs["autocorrect"], attrs["autocapitalize"], attrs["spellcheck"] = "on", "sentences", "true"
	}
	for k, v := range attrs {
		s.input.Call("setAttribute", k, v)
	}
	w.el.Call("appendChild", s.canvas)
	w.el.Call("appendChild", s.input)
	s.area = s.input
	if s.native {
		s.newSignInForm(doc)
	}
	ctx, err := gl.NewContext(s.canvas)
	if err != nil {
		log.Printf("mygo: drawing in memory: %v", err)
		s.ctx2d = s.canvas.Call("getContext", "2d")
	} else {
		s.gl = ctx
	}
	s.frame = js.FuncOf(func(js.Value, []js.Value) any {
		w.b.sync(s.drawFrame)
		return nil
	})
	s.listen()
	return s, nil
}

// on adds a listener of the browser's events to target, which handles them
// on the main goroutine.
func (s *surface) on(target js.Value, event string, passive bool, fn func(e js.Value)) {
	f := js.FuncOf(func(_ js.Value, args []js.Value) any {
		s.w.b.sync(func() {
			if !s.closed {
				fn(args[0])
			}
		})
		return nil
	})
	s.funcs = append(s.funcs, f)
	target.Call("addEventListener", event, f, map[string]any{"passive": passive})
	// Listeners on window and document go with the surface.
	if target.Equal(js.Global()) || target.Equal(js.Global().Get("document")) {
		s.global = append(s.global, listener{target, event, f})
	}
}

type listener struct {
	target js.Value
	event  string
	fn     js.Func
}

func (s *surface) send(ev platform.SurfaceEvent) bool { return s.w.h.SurfaceEvent(ev) }

func (s *surface) listen() {
	c, in := s.canvas, s.input
	ro := js.Global().Get("ResizeObserver")
	if !ro.IsUndefined() {
		f := js.FuncOf(func(js.Value, []js.Value) any {
			s.w.b.sync(func() {
				if !s.closed && s.measure() {
					s.RequestFrame()
				}
			})
			return nil
		})
		s.funcs = append(s.funcs, f)
		s.observer = ro.New(f)
		s.observer.Call("observe", c)
	}
	s.on(js.Global(), "resize", true, func(js.Value) {
		if s.measure() {
			s.RequestFrame()
		}
	})
	s.on(js.Global().Get("document"), "visibilitychange", true, func(js.Value) {
		if !s.Occluded() {
			s.send(platform.SurfaceEvent{Kind: platform.SurfaceShown})
			s.RequestFrame()
		}
	})
	if s.gl != nil {
		s.on(c, "webglcontextlost", false, func(e js.Value) { e.Call("preventDefault") })
		s.on(c, "webglcontextrestored", true, func(js.Value) {
			s.gl.Renew()
			s.presenter = gl.Presenter{}
			s.send(platform.SurfaceEvent{Kind: platform.SurfaceRenew})
		})
	}

	s.listenTouch(c)
	s.on(c, "pointermove", true, func(e js.Value) {
		if s.touchEvent(e) {
			return
		}
		x, y := s.point(e)
		s.send(platform.SurfaceEvent{Kind: platform.PointerMove, X: x, Y: y, Mods: s.mods(e)})
	})
	s.on(c, "pointerdown", false, func(e js.Value) {
		e.Call("preventDefault")
		s.focus()
		if s.touchEvent(e) {
			return
		}
		x, y := s.point(e)
		button := e.Get("button").Int()
		switch button {
		case 3, 4:
			k := platform.KeyBack
			if button == 4 {
				k = platform.KeyForward
			}
			s.send(platform.SurfaceEvent{Kind: platform.KeyPressed, Key: k, Mods: s.mods(e)})
			s.send(platform.SurfaceEvent{Kind: platform.KeyReleased, Key: k, Mods: s.mods(e)})
			return
		}
		c.Call("setPointerCapture", e.Get("pointerId"))
		b := buttonOf(button)
		s.downX, s.downY = x, y
		clicks := s.clicks.Press(b, x, y, uint32(e.Get("timeStamp").Float()), platform.DefaultDoubleClick) // browsers tell no double-click settings
		s.send(platform.SurfaceEvent{Kind: platform.PointerDown, X: x, Y: y, Button: b, Clicks: clicks, Mods: s.mods(e)})
	})
	s.on(c, "pointerup", true, func(e js.Value) {
		if s.touchEvent(e) {
			return
		}
		button := e.Get("button").Int()
		if button == 3 || button == 4 {
			return
		}
		x, y := s.point(e)
		s.send(platform.SurfaceEvent{Kind: platform.PointerUp, X: x, Y: y, Button: buttonOf(button), Mods: s.mods(e)})
	})
	s.on(c, "pointercancel", true, func(e js.Value) {
		if s.touchEvent(e) {
			return
		}
		x, y := s.point(e)
		s.send(platform.SurfaceEvent{Kind: platform.PointerUp, X: x, Y: y, Button: buttonOf(e.Get("button").Int()), Mods: s.mods(e)})
	})
	s.on(c, "pointerleave", true, func(js.Value) {
		s.send(platform.SurfaceEvent{Kind: platform.PointerLeave})
	})
	// iOS sends a tap's mouse events even though its pointerdown was
	// canceled: the mousedown would move the focus to the canvas, and the
	// touch keyboard would hide as soon as it showed.
	s.on(c, "mousedown", false, func(e js.Value) { e.Call("preventDefault") })
	s.on(c, "contextmenu", false, func(e js.Value) { e.Call("preventDefault") })
	s.on(c, "wheel", false, func(e js.Value) {
		e.Call("preventDefault")
		x, y := s.point(e)
		dx, dy := e.Get("deltaX").Float(), e.Get("deltaY").Float()
		precise := true
		switch e.Get("deltaMode").Int() {
		case 1: // lines
			dx, dy, precise = dx*lineHeight, dy*lineHeight, false
		case 2: // pages
			dx, dy, precise = dx*s.width, dy*s.height, false
		default:
			// A wheel's notches, which browsers give as pixels too.
			if w := e.Get("wheelDeltaY"); w.Type() == js.TypeNumber && w.Int() != 0 && w.Int()%120 == 0 {
				precise = false
			}
		}
		if precise && e.Get("ctrlKey").Bool() {
			// A touchpad's pinch, which Chrome and Firefox send as
			// scrolling with Control held, as they do Control and two
			// fingers: Chrome scrolls 100 pixels by the natural log of
			// the scale.
			s.send(platform.SurfaceEvent{Kind: platform.PointerZoom, X: x, Y: y, Scale: math.Exp(-dy / 100), Mods: s.mods(e)})
			return
		}
		s.send(platform.SurfaceEvent{Kind: platform.PointerScroll, X: x, Y: y, DX: dx, DY: dy, Precise: precise, Mods: s.mods(e)})
	})
	// Safari sends pinches as gesture events instead, whose scale is from
	// 1 as the pinch began; without preventDefault it zooms the page.
	pinch := 1.0
	gesture := func(e js.Value) {
		e.Call("preventDefault")
		scale := e.Get("scale").Float()
		if e.Get("type").String() == "gesturestart" {
			pinch = 1
		}
		if scale > 0 && pinch > 0 && scale != pinch {
			x, y := s.point(e)
			s.send(platform.SurfaceEvent{Kind: platform.PointerZoom, X: x, Y: y, Scale: scale / pinch, Mods: s.mods(e)})
		}
		pinch = scale
	}
	s.on(c, "gesturestart", false, gesture)
	s.on(c, "gesturechange", false, gesture)

	// Files dragged over the canvas, and dropped: their bytes are read, as
	// browsers give them, before the drop goes to the content (files.go).
	// The browser opens no file dropped on the window, which would leave
	// the app, whether the content takes it or not.
	files := func(e js.Value) bool {
		types := e.Get("dataTransfer").Get("types")
		return !types.IsNull() && !types.IsUndefined() && types.Call("includes", "Files").Bool()
	}
	over := func(e js.Value) {
		if !files(e) {
			return
		}
		e.Call("preventDefault")
		x, y := s.point(e)
		effect := "none"
		if s.send(platform.SurfaceEvent{Kind: platform.FileDragOver, X: x, Y: y}) {
			effect = "copy"
		}
		e.Get("dataTransfer").Set("dropEffect", effect)
	}
	s.on(c, "dragenter", false, over)
	s.on(c, "dragover", false, over)
	s.on(c, "dragleave", true, func(e js.Value) {
		if files(e) {
			s.send(platform.SurfaceEvent{Kind: platform.FileDragLeave})
		}
	})
	s.on(c, "drop", false, func(e js.Value) {
		if !files(e) {
			return
		}
		e.Call("preventDefault")
		x, y := s.point(e)
		list := e.Get("dataTransfer").Get("files")
		names := make([]string, list.Length())
		reads := make([]any, list.Length())
		for i := range names {
			f := list.Index(i)
			names[i] = f.Get("name").String()
			reads[i] = f.Call("arrayBuffer")
		}
		var done js.Func
		done = js.FuncOf(func(_ js.Value, args []js.Value) any {
			done.Release()
			results := args[0]
			s.w.b.sync(func() {
				// Folders have no bytes to read: they are left out.
				var kept []string
				var data [][]byte
				for i, name := range names {
					r := results.Index(i)
					if r.Get("status").String() != "fulfilled" {
						continue
					}
					buf := js.Global().Get("Uint8Array").New(r.Get("value"))
					b := make([]byte, buf.Length())
					js.CopyBytesToGo(b, buf)
					kept, data = append(kept, name), append(data, b)
				}
				if s.closed {
					return
				}
				if len(kept) == 0 {
					s.send(platform.SurfaceEvent{Kind: platform.FileDragLeave})
					return
				}
				s.send(platform.SurfaceEvent{Kind: platform.FileDrop, X: x, Y: y, Files: drops.add(kept, data)})
			})
			return nil
		})
		js.Global().Get("Promise").Call("allSettled", reads).Call("then", done)
	})

	s.listenInput(in)
	// on touch devices, a sign-in form's fields have inputs of
	// their own kind (nativeKinds), for the system's password autofill
	if s.user.Truthy() {
		s.listenInput(s.user)
		s.listenInput(s.pass)
	}
}

// listenInput handles the events of one of the elements that take the
// keyboard (the textarea, or on touch devices the inputs of a sign-in
// form). Only the active one, s.input, edits the content's text.
func (s *surface) listenInput(in js.Value) {
	s.on(in, "focus", true, func(js.Value) {
		if s.switching || !in.Equal(s.input) {
			return
		}
		s.w.focused = true
		s.w.h.Focused()
		s.send(platform.SurfaceEvent{Kind: platform.SurfaceFocus})
	})
	s.on(in, "blur", true, func(js.Value) {
		if s.switching || !in.Equal(s.input) {
			return
		}
		s.w.focused = false
		s.send(platform.SurfaceEvent{Kind: platform.SurfaceBlur})
		s.w.h.Blurred()
	})
	s.on(in, "keydown", false, func(e js.Value) { s.key(e, true) })
	s.on(in, "keyup", false, func(e js.Value) { s.key(e, false) })
	s.on(in, "beforeinput", false, func(e js.Value) {
		if s.nativeEditing() {
			switch e.Get("inputType").String() {
			case "insertLineBreak", "insertParagraph", "historyUndo", "historyRedo":
				e.Call("preventDefault")
			}
			return
		}
		switch e.Get("inputType").String() {
		case "insertText":
			if !e.Get("isComposing").Bool() && !s.composing {
				e.Call("preventDefault")
				if t := e.Get("data"); t.Type() == js.TypeString && t.String() != "" {
					s.send(platform.SurfaceEvent{Kind: platform.TextInput, Text: t.String()})
				}
			}
		case "insertCompositionText", "deleteCompositionText", "insertFromComposition":
		default:
			// Keys did what the rest do (Enter, Backspace, …); a paste
			// comes as its own event.
			e.Call("preventDefault")
		}
	})
	s.on(in, "input", true, func(e js.Value) {
		if !in.Equal(s.input) {
			// the system filled the other field of a sign-in form (password autofill)
			s.nativeAutofill(in)
			return
		}
		if s.nativeEditing() && !s.composing && !e.Get("isComposing").Bool() {
			s.nativeSync()
		}
	})
	s.on(in, "compositionstart", true, func(js.Value) { s.composing = true })
	s.on(in, "compositionupdate", true, func(e js.Value) {
		t := e.Get("data").String()
		if s.nativeEditing() {
			// the composition shows in the field, where the textarea has it (marked text)
			s.nativeCompose(t)
			return
		}
		s.send(platform.SurfaceEvent{Kind: platform.TextComposition, Text: t, Caret: utf8.RuneCountInString(t)})
	})
	s.on(in, "compositionend", true, func(e js.Value) {
		s.composing = false
		if s.nativeEditing() {
			if c := s.text.Client; c != nil {
				// the composition is now the field's text: compare the textarea
				// with that, so only a difference (none, usually) is applied
				c.UnmarkText()
				s.mirror, _ = c.TextForRange(platform.TextRange{Start: 0, End: math.MaxInt32})
			}
			s.nativeSync()
			return
		}
		s.send(platform.SurfaceEvent{Kind: platform.TextComposition})
		if t := e.Get("data"); t.Type() == js.TypeString && t.String() != "" {
			s.send(platform.SurfaceEvent{Kind: platform.TextInput, Text: t.String()})
		}
		in.Set("value", "")
	})
	// A tap on the field moves the content's caret, as a tap on the canvas
	// does; a long press is the system's (its menu, selecting a word).
	s.on(in, "pointerdown", true, func(e js.Value) {
		s.pressedAt = e.Get("timeStamp").Float()
		s.pressX, s.pressY = s.point(e)
	})
	s.on(in, "pointerup", true, func(e js.Value) {
		x, y := s.point(e)
		if e.Get("timeStamp").Float()-s.pressedAt > 350 || math.Abs(x-s.pressX)+math.Abs(y-s.pressY) > 10 {
			return
		}
		clicks := s.clicks.Press(0, x, y, uint32(e.Get("timeStamp").Float()), platform.DefaultDoubleClick)
		s.send(platform.SurfaceEvent{Kind: platform.PointerDown, X: x, Y: y, Button: 0, Clicks: clicks})
		s.send(platform.SurfaceEvent{Kind: platform.PointerUp, X: x, Y: y, Button: 0})
	})
	s.on(in, "copy", false, func(e js.Value) {
		if s.nativeEditing() && s.nativeSelected() {
			return // the system copies the textarea's selection: the field's text
		}
		s.copy(e, "copy")
	})
	s.on(in, "cut", false, func(e js.Value) {
		if s.nativeEditing() && s.nativeSelected() {
			return // and cuts it; the input event that follows edits the field
		}
		s.copy(e, "cut")
	})
	s.on(in, "paste", false, func(e js.Value) {
		if s.nativeEditing() {
			return // the browser pastes into the textarea; the input event edits the field
		}
		e.Call("preventDefault")
		data := e.Get("clipboardData")
		if data.IsNull() || data.IsUndefined() {
			return
		}
		// The text formats pasted: plain text, HTML, and those of apps.
		var reps []transfer.Representation
		types := data.Get("types")
		for i := range types.Length() {
			f := types.Index(i).String()
			if f == "Files" {
				continue
			}
			if v := data.Call("getData", f).String(); v != "" {
				reps = append(reps, transfer.Bytes(transfer.Format(f), []byte(v)))
			}
		}
		s.w.b.mu.Lock()
		s.w.b.clip = data.Call("getData", "text/plain").String()
		s.w.b.clipData = transfer.Data{}
		if len(reps) > 0 {
			s.w.b.clipData = transfer.New(transfer.NewItem(reps...))
		}
		s.w.b.mu.Unlock()
		s.send(platform.SurfaceEvent{Kind: platform.SurfaceCommand, Text: "paste"})
	})
}

// lineHeight is how many DIPs a line of a wheel scrolls.
const lineHeight = 16

// copy has the content copy, or cut, its selection, and gives the browser
// the text, which the clipboard event carries to the system's clipboard.
func (s *surface) copy(e js.Value, cmd string) {
	e.Call("preventDefault")
	if !s.copied {
		s.send(platform.SurfaceEvent{Kind: platform.SurfaceCommand, Text: cmd})
	}
	s.copied = false
	s.w.b.mu.Lock()
	text, all := s.w.b.clip, s.w.b.clipData
	s.w.b.mu.Unlock()
	if data := e.Get("clipboardData"); !data.IsNull() && !data.IsUndefined() && text != "" {
		data.Call("setData", "text/plain", text)
		// The other text formats copied, as a rich text's HTML.
		for _, f := range all.Formats() {
			if f == transfer.Text || f == transfer.FileList || f == transfer.PNG {
				continue
			}
			if b, err := all.Read(f); err == nil {
				data.Call("setData", string(f), string(b))
			}
		}
	}
}

func buttonOf(b int) int {
	switch b {
	case 1:
		return 2
	case 2:
		return 1
	}
	return 0
}

// point returns where a pointer event happened in the canvas, in DIPs.
func (s *surface) point(e js.Value) (float64, float64) {
	r := s.canvas.Call("getBoundingClientRect")
	return e.Get("clientX").Float() - r.Get("left").Float(), e.Get("clientY").Float() - r.Get("top").Float()
}

// mods returns the modifiers of an event. Command is Control on a Mac, as
// the shortcuts of package ui, which take a browser for another system
// than macOS, expect Control where Macs have Command.
func (s *surface) mods(e js.Value) platform.Modifiers {
	var m platform.Modifiers
	if e.Get("shiftKey").Bool() {
		m |= platform.ModShift
	}
	if e.Get("altKey").Bool() {
		m |= platform.ModAlt
	}
	ctrl, meta := e.Get("ctrlKey").Bool(), e.Get("metaKey").Bool()
	if s.w.b.mac {
		ctrl, meta = meta, ctrl
	}
	if ctrl {
		m |= platform.ModCtrl
	}
	if meta {
		m |= platform.ModSuper
	}
	return m
}

// key handles a key going down or up in the textarea. Keys that type text
// come as text from beforeinput, after this, and keys an input method
// takes are its own. The browser does nothing of its own with keys but
// those typing text, copying and pasting, and those of its own (F5, F11,
// F12, …).
func (s *surface) key(e js.Value, down bool) {
	if down {
		s.copied = false
	}
	if e.Get("isComposing").Bool() || e.Get("keyCode").Int() == 229 {
		return
	}
	name := e.Get("key").String()
	mods := s.mods(e)
	switch name {
	case "Shift", "Control", "Alt", "Meta", "CapsLock", "AltGraph":
		s.send(platform.SurfaceEvent{Kind: platform.ModifiersChanged, Mods: mods})
		return
	case "Dead", "Process", "Unidentified", "Compose":
		return
	}
	code := e.Get("code").String()
	k, physical := keyOf(name, code), codeKey(code)
	primary := mods&platform.ModCtrl != 0
	if s.nativeEditing() && !primary && (name == "Backspace" || name == "Delete" || utf8.RuneCountInString(name) == 1) {
		return // the browser edits the textarea, and the input event the field
	}
	if down && primary && mods&(platform.ModAlt|platform.ModShift) == 0 {
		switch k {
		case platform.KeyV:
			// The paste event brings the clipboard's text.
			return
		case platform.KeyC, platform.KeyX:
			// The content copies now, and the copy event that follows
			// gives the browser the text.
			s.send(platform.SurfaceEvent{Kind: platform.KeyPressed, Key: k, PhysicalKey: physical, Mods: mods, Repeat: e.Get("repeat").Bool()})
			s.copied = true
			return
		}
	}
	kind := platform.KeyReleased
	if down {
		kind = platform.KeyPressed
	}
	s.send(platform.SurfaceEvent{Kind: kind, Key: k, PhysicalKey: physical, Mods: mods, Repeat: e.Get("repeat").Bool()})
	if !down {
		return
	}
	switch {
	case name == "F5" || name == "F11" || name == "F12":
	case utf8.RuneCountInString(name) > 1 || primary:
		e.Call("preventDefault")
	}
}

// keyOf returns the key of a KeyboardEvent's key and code: letters and
// digits by what they type, as other systems tell them.
func keyOf(name, code string) platform.Key {
	if utf8.RuneCountInString(name) == 1 {
		r, _ := utf8.DecodeRuneInString(name)
		if k := platform.KeyForRune(r); k != platform.KeyUnknown {
			return k
		}
	}
	if k, ok := named[name]; ok {
		return k
	}
	return codeKey(code)
}

// codeKey returns the key at a KeyboardEvent's code: its position on an
// ANSI US keyboard (SurfaceEvent.PhysicalKey).
func codeKey(code string) platform.Key {
	switch {
	case len(code) == 4 && code[:3] == "Key":
		return platform.KeyA + platform.Key(code[3]-'A')
	case len(code) == 6 && code[:5] == "Digit":
		return platform.Key0 + platform.Key(code[5]-'0')
	}
	if k, ok := codes[code]; ok {
		return k
	}
	return platform.KeyUnknown
}

var named = map[string]platform.Key{
	"Enter": platform.KeyEnter, "Escape": platform.KeyEscape, "Backspace": platform.KeyBackspace, "Tab": platform.KeyTab,
	" ": platform.KeySpace, "Delete": platform.KeyDelete, "Insert": platform.KeyInsert, "Home": platform.KeyHome, "End": platform.KeyEnd,
	"PageUp": platform.KeyPageUp, "PageDown": platform.KeyPageDown, "ArrowLeft": platform.KeyLeft, "ArrowRight": platform.KeyRight,
	"ArrowUp": platform.KeyUp, "ArrowDown": platform.KeyDown, "ContextMenu": platform.KeyContextMenu,
	"BrowserBack": platform.KeyBack, "BrowserForward": platform.KeyForward,
	"F1": platform.KeyF1, "F2": platform.KeyF2, "F3": platform.KeyF3, "F4": platform.KeyF4, "F5": platform.KeyF5, "F6": platform.KeyF6,
	"F7": platform.KeyF7, "F8": platform.KeyF8, "F9": platform.KeyF9, "F10": platform.KeyF10, "F11": platform.KeyF11, "F12": platform.KeyF12,
}

var codes = map[string]platform.Key{
	"Minus": platform.KeyMinus, "Equal": platform.KeyEqual, "Comma": platform.KeyComma, "Period": platform.KeyPeriod,
	"Slash": platform.KeySlash, "Semicolon": platform.KeySemicolon, "Quote": platform.KeyQuote, "BracketLeft": platform.KeyBracketLeft,
	"BracketRight": platform.KeyBracketRight, "Backslash": platform.KeyBackslash, "Backquote": platform.KeyBackquote, "Space": platform.KeySpace,
}

// focus gives the window the keyboard: its textarea.
func (s *surface) focus() {
	s.input.Call("focus", map[string]any{"preventScroll": true})
}

// measure takes the canvas's size and the device pixel ratio, and tells the
// content when they changed.
func (s *surface) measure() bool {
	r := s.canvas.Call("getBoundingClientRect")
	w, h, scale := r.Get("width").Float(), r.Get("height").Float(), devicePixelRatio()
	if w == s.width && h == s.height && scale == s.scale {
		return false
	}
	s.width, s.height, s.scale = w, h, scale
	s.send(platform.SurfaceEvent{Kind: platform.SurfaceResize})
	return true
}

// pixels returns the size of frames, as package ui computes it.
func (s *surface) pixels() (int, int) {
	return int(math.Ceil(float64(float32(s.width) * float32(s.scale)))), int(math.Ceil(float64(float32(s.height) * float32(s.scale))))
}

// drawFrame handles a frame of the browser's, when the content asked for
// one, with the canvas as large as the frame and WebGL's context current.
func (s *surface) drawFrame() {
	s.pending = false
	if s.closed || !s.w.visible {
		return
	}
	s.measure()
	pw, ph := s.pixels()
	if s.canvas.Get("width").Int() != pw || s.canvas.Get("height").Int() != ph {
		s.canvas.Set("width", pw)
		s.canvas.Set("height", ph)
	}
	if s.gl != nil {
		s.gl.MakeCurrent()
	}
	s.send(platform.SurfaceEvent{Kind: platform.SurfaceFrame})
}

func (s *surface) Native() platform.SurfaceNative {
	if s.gl == nil {
		return platform.SurfaceNative{}
	}
	return platform.SurfaceNative{Canvas: s.gl.ID()}
}

func (s *surface) Size() (float64, float64, float64) { return s.width, s.height, s.scale }

func (s *surface) RequestFrame() {
	if s.pending || s.closed {
		return
	}
	s.pending = true
	s.request = js.Global().Call("requestAnimationFrame", s.frame)
}

// RefreshRate is unknown: browsers do not tell it.
func (s *surface) RefreshRate() float64 { return 0 }

// Occluded reports whether the page is hidden, as in a tab in the
// background, where browsers give no frames.
func (s *surface) Occluded() bool { return js.Global().Get("document").Get("hidden").Bool() }

func (s *surface) PresentPixels(pix []byte, stride, width, height int) {
	if s.gl != nil {
		if err := s.presenter.Present(pix, stride, width, height); err != nil {
			log.Print("mygo: ", err)
		}
		return
	}
	if s.ctx2d.IsUndefined() || s.ctx2d.IsNull() || width <= 0 || height <= 0 {
		return
	}
	// ImageData holds straight RGBA; frames are premultiplied BGRA.
	n := 4 * width * height
	if cap(s.rgba) < n {
		s.rgba = make([]byte, n)
	}
	rgba := s.rgba[:n]
	for y := range height {
		src, dst := pix[y*stride:][:4*width], rgba[4*width*y:][:4*width]
		for x := 0; x < 4*width; x += 4 {
			b, g, r, a := src[x], src[x+1], src[x+2], src[x+3]
			if a != 0 && a != 255 {
				r, g, b = unpremul(r, a), unpremul(g, a), unpremul(b, a)
			}
			dst[x], dst[x+1], dst[x+2], dst[x+3] = r, g, b, a
		}
	}
	if s.image.IsUndefined() || s.image.Get("width").Int() != width || s.image.Get("height").Int() != height {
		s.image = s.ctx2d.Call("createImageData", width, height)
	}
	js.CopyBytesToJS(s.image.Get("data"), rgba)
	s.ctx2d.Call("putImageData", s.image, 0, 0)
}

func unpremul(c, a byte) byte { return byte(min(255, (int(c)*255+int(a)/2)/int(a))) }

var cursors = [...]string{
	platform.CursorDefault: "default", platform.CursorPointer: "pointer", platform.CursorText: "text", platform.CursorMove: "move",
	platform.CursorResizeEW: "ew-resize", platform.CursorResizeNS: "ns-resize", platform.CursorResizeNWSE: "nwse-resize",
	platform.CursorResizeNESW: "nesw-resize", platform.CursorNotAllowed: "not-allowed", platform.CursorCrosshair: "crosshair",
	platform.CursorGrab: "grab", platform.CursorGrabbing: "grabbing", platform.CursorResizeN: "n-resize", platform.CursorResizeE: "e-resize",
	platform.CursorResizeS: "s-resize", platform.CursorResizeW: "w-resize", platform.CursorResizeColumn: "col-resize",
	platform.CursorResizeRow: "row-resize", platform.CursorVerticalText: "vertical-text", platform.CursorCopy: "copy",
	platform.CursorAlias: "alias", platform.CursorContextMenu: "context-menu", platform.CursorNone: "none",
}

func (s *surface) SetCursor(c platform.Cursor) {
	name := "default"
	if int(c) < len(cursors) && cursors[c] != "" {
		name = cursors[c]
	}
	s.canvas.Get("style").Set("cursor", name)
}

// SetTextInput moves the textarea to the caret, where input methods show
// their candidates, and lets touch devices show their keyboard while an
// element takes text: browsers show it, or hide it, as the inputmode of
// the focused textarea changes. Keys of a hardware keyboard come either
// way.
func (s *surface) SetTextInput(t platform.TextInputState) {
	if s.native {
		s.nativeSwitch(s.nativeInputFor(t)) // first: the keyboard's element for the field's kind
	}
	mode := "none"
	if t.Active {
		mode = "text"
		if s.input.Equal(s.user) {
			if m := s.user.Call("getAttribute", "inputmode").String(); m == "numeric" || m == "email" {
				mode = m
			}
		}
	}
	if s.input.Call("getAttribute", "inputmode").String() != mode {
		s.input.Call("setAttribute", "inputmode", mode)
	}
	s.text = t
	st := s.input.Get("style")
	if s.native {
		s.nativeLayout(t)
	}
	if !t.Active {
		st.Set("left", "0")
		st.Set("top", "0")
		return
	}
	if s.nativeEditing() {
		return
	}
	st.Set("left", ftoa(t.Caret.X)+"px")
	st.Set("top", ftoa(t.Caret.Y)+"px")
	st.Set("height", ftoa(max(t.Caret.H, 1))+"px")
}

// ---- touch devices: the field's own textarea ----

// newSignInForm makes the inputs a sign-in form's fields use on touch
// devices: a username and a password input in a real form, as the system's
// password autofill and strong-password suggestions need them, to fill both
// fields at once. They lie out of sight like the textarea until a field of
// their kind has the keyboard (nativeInputFor).
func (s *surface) newSignInForm(doc js.Value) {
	form := doc.Call("createElement", "form")
	form.Call("setAttribute", "autocomplete", "on")
	form.Get("style").Set("cssText", "position:absolute;left:0;top:0;width:0;height:0;margin:0;padding:0;overflow:visible")
	s.on(form, "submit", false, func(e js.Value) { e.Call("preventDefault") })
	mk := func(typ, ac string) js.Value {
		in := doc.Call("createElement", "input")
		for k, v := range map[string]string{"type": typ, "autocomplete": ac, "autocorrect": "off", "autocapitalize": "off", "spellcheck": "false", "tabindex": "-1", "inputmode": "none"} {
			in.Call("setAttribute", k, v)
		}
		in.Get("style").Set("cssText", "position:absolute;left:0;top:0;width:1px;height:1em;padding:0;margin:0;border:0;outline:none;opacity:0;color:transparent;background:transparent;caret-color:transparent;font-size:16px;z-index:-1")
		form.Call("appendChild", in)
		return in
	}
	s.user = mk("text", "username")
	s.pass = mk("password", "current-password")
	s.w.el.Call("appendChild", form)
}

// nativeInputFor returns the element a field of t's kind uses on a touch
// device, set up for that kind: the password input for a password, the
// username input for a username, email or one-time code, else the textarea.
func (s *surface) nativeInputFor(t platform.TextInputState) js.Value {
	if !s.user.Truthy() || !t.Active || t.Client == nil {
		return s.area
	}
	switch {
	case t.Secure:
		ac := t.ContentType
		if ac != "new-password" {
			ac = "current-password"
		}
		s.pass.Call("setAttribute", "autocomplete", ac)
		return s.pass
	case t.ContentType == "username" || t.ContentType == "email" || t.ContentType == "email username" || t.ContentType == "one-time-code":
		// type=text with an email keyboard: email inputs don't let a page
		// read or set their selection, which mirroring the field needs
		ac, mode := t.ContentType, "text"
		switch t.ContentType {
		case "email":
			mode = "email"
		case "email username":
			ac, mode = "username", "email"
		case "one-time-code":
			mode = "numeric"
		}
		s.user.Call("setAttribute", "autocomplete", ac)
		s.user.Call("setAttribute", "inputmode", mode)
		return s.user
	}
	return s.area
}

// nativeSwitch gives the keyboard to el (inside the press that focused the
// field, so a touch keyboard shows): the element of the field's kind.
func (s *surface) nativeSwitch(el js.Value) {
	if el.Equal(s.input) {
		return
	}
	old := s.input
	s.switching = true
	s.input = el
	s.mirror, s.pushed = "\x00", platform.TextRange{Start: -1} // write the field's text into it
	if s.w.focused {
		el.Call("focus", map[string]any{"preventScroll": true})
	}
	st := old.Get("style")
	st.Set("zIndex", "-1")
	st.Set("width", "1px")
	st.Set("transform", "none")
	if old.Equal(s.area) || old.Equal(s.pass) {
		old.Set("value", "") // no field's text (no password) left behind
	}
	s.switching = false
}

// nativeAutofill: the system filled the sign-in form's input that doesn't
// have the keyboard; its field gets the text.
func (s *surface) nativeAutofill(in js.Value) {
	kind := "username"
	if in.Equal(s.pass) {
		kind = "password"
	} else if !in.Equal(s.user) {
		return
	}
	s.send(platform.SurfaceEvent{Kind: platform.TextAutofill, ContentType: kind, Text: in.Get("value").String()})
}

// nativeEditing: the textarea is the focused field's, on a touch device.
func (s *surface) nativeEditing() bool {
	return s.native && s.text.Active && s.text.Client != nil && s.text.Field.W > 0
}

// nativeLayout lays the textarea over the field, with its text and
// selection, or back out of sight.
func (s *surface) nativeLayout(t platform.TextInputState) {
	st := s.input.Get("style")
	if !(t.Active && t.Client != nil && t.Field.W > 0) {
		st.Set("width", "1px")
		st.Set("zIndex", "-1")
		st.Set("color", "transparent")
		st.Set("transform", "none")
		if s.mirror != "" {
			s.input.Set("value", "") // no field's text left behind
		}
		s.mirror, s.pushed = "", platform.TextRange{}
		return
	}
	f := t.Field
	// At least a finger tall (44 DIPs, as iOS asks of touch targets), centered
	// on the field, so a long press anywhere on it reaches the textarea.
	if f.H < 44 {
		f.Y -= (44 - f.H) / 2
		f.H = 44
	}
	// Its text where the field draws it, so the system's selection and
	// handles land near the field's own glyphs; the field draws the text.
	// Its font is 16px at least, scaled down to the field's size: iOS zooms
	// the page into a focused field whose text is smaller.
	line := max(t.Caret.H, 1)
	padX, padY := 0.0, 0.0
	if b, _, ok := t.Client.BoundsForRange(platform.TextRange{}); ok {
		padX, padY = max(0, b.X-f.X), max(0, b.Y-f.Y)
	}
	font, k := math.Round(line/1.3), 1.0
	if font < 16 {
		k, font = font/16, 16
	}
	for key, v := range map[string]string{
		"left": ftoa(f.X) + "px", "top": ftoa(f.Y) + "px", "width": ftoa(f.W/k) + "px", "height": ftoa(f.H/k) + "px",
		"transform": "scale(" + ftoa(k) + ")", "transformOrigin": "0 0", "touchAction": "manipulation",
		"zIndex": "2", "opacity": "1", "color": "transparent", "caretColor": "transparent", "background": "transparent",
		"webkitTextFillColor": "transparent", "whiteSpace": "pre-wrap", "overflow": "hidden",
		"fontSize": ftoa(font) + "px", "lineHeight": ftoa(line/k) + "px",
		"padding": ftoa(padY/k) + "px 0 0 " + ftoa(padX/k) + "px", "boxSizing": "border-box", "fontFamily": "system-ui,-apple-system,sans-serif",
		"webkitUserSelect": "text", "userSelect": "text", "webkitTouchCallout": "default",
	} {
		st.Set(key, v)
	}
	if s.composing {
		return
	}
	text, _ := t.Client.TextForRange(platform.TextRange{Start: 0, End: math.MaxInt32})
	sel := t.Client.Selection().Range
	if text != s.mirror {
		s.input.Set("value", text)
		s.mirror = text
		s.pushed = platform.TextRange{Start: -1}
	}
	// The field's selection moved (a tap, an arrow key, its own edit): the
	// textarea's follows. The textarea's own moves (its handles) stay.
	if sel != s.pushed {
		setSelection(s.input, sel.Start, sel.End)
		s.pushed = sel
	}
}

// nativeSync gives the field what the browser changed in the textarea: the
// runs that differ from the text it last held, as one replacement.
func (s *surface) nativeSync() {
	c := s.text.Client
	if c == nil {
		return
	}
	now := s.input.Get("value").String()
	from, to, text, ok := textDiff(s.mirror, now)
	s.mirror = now
	if !ok {
		return
	}
	c.ReplaceText(&platform.TextRange{Start: from, End: to}, text)
	// the caret where the browser left it
	st, en := selectionOf(s.input)
	s.pushed = platform.TextRange{Start: st, End: en}
}

// nativeCompose shows an input method's composition in the field as marked
// text, over what it replaces in the textarea.
func (s *surface) nativeCompose(t string) {
	c := s.text.Client
	if c == nil {
		return
	}
	n := utf16Len(t)
	c.SetMarkedText(nil, t, platform.TextRange{Start: n, End: n})
}

// nativeSelected: the textarea has a selection of its own (from the
// system's menu or handles).
func (s *surface) nativeSelected() bool {
	st, en := selectionOf(s.input)
	return en > st
}

func (s *surface) UpdateAccessibility(*platform.AccessTree)  {}
func (s *surface) StartDataDrag(platform.DragRequest)        {}
func (s *surface) CancelDataDrag()                           {}
func (s *surface) SetDropFormats([]transfer.Format)          {}
func (s *surface) PlaceWebViews([]platform.WebViewPlacement) {}

func (s *surface) NewWebView(*platform.WindowOptions, platform.WindowHandler) (platform.WebView, error) {
	return nil, errUnsupported
}

func (s *surface) close() {
	s.closed = true
	if s.pending {
		js.Global().Call("cancelAnimationFrame", s.request)
	}
	if !s.observer.IsUndefined() {
		s.observer.Call("disconnect")
	}
	for _, l := range s.global {
		l.target.Call("removeEventListener", l.event, l.fn)
	}
	// The functions may still be called back once: they find the surface
	// closed. They are released after the frame.
	funcs, frame := s.funcs, s.frame
	var release js.Func
	release = js.FuncOf(func(js.Value, []js.Value) any {
		for _, f := range funcs {
			f.Release()
		}
		frame.Release()
		release.Release()
		return nil
	})
	js.Global().Call("setTimeout", release, 1000)
}

func itoa(n int) string     { return strconv.Itoa(n) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// setSelectionRange and the selection's ends of an element that may not
// have one (a browser throws, or answers null, for some input types): never
// a panic.
var trySelect = js.Global().Get("Function").New("el", "a", "b", "try { el.setSelectionRange(a, b) } catch (e) {}")

func setSelection(el js.Value, start, end int) { trySelect.Invoke(el, start, end) }

func selectionOf(el js.Value) (int, int) {
	a, b := el.Get("selectionStart"), el.Get("selectionEnd")
	if a.Type() != js.TypeNumber || b.Type() != js.TypeNumber {
		return 0, 0
	}
	return a.Int(), b.Int()
}
