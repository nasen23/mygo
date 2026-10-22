//go:build js

package web

import (
	"math"
	"strconv"
	"syscall/js"

	"github.com/egoist/mygo/internal/platform"
)

// listenTouch makes the timers a finger on the canvas uses.
func (s *surface) listenTouch(c js.Value) {
	t := &s.touch
	t.holdFunc = js.FuncOf(func(js.Value, []js.Value) any {
		s.w.b.sync(func() {
			t.hold = js.Undefined()
			if !s.closed && t.id != 0 && !t.scrolling && !t.pressed {
				// A long press selects the word there, as iOS's does; the
				// finger's moves then select more, by words.
				t.pressed = true
				s.downX, s.downY = t.startX, t.startY
				s.send(platform.SurfaceEvent{Kind: platform.PointerDown, X: t.startX, Y: t.startY, Clicks: 1, Hold: true})
			}
		})
		return nil
	})
	t.flingFn = js.FuncOf(func(_ js.Value, args []js.Value) any {
		s.w.b.sync(func() {
			t.fling = js.Undefined()
			if !s.closed {
				s.touchFling(args[0].Float())
			}
		})
		return nil
	})
	s.funcs = append(s.funcs, t.holdFunc, t.flingFn)
}

// touchEvent follows a finger's pointer event on the canvas, and reports
// whether it took it: a mouse's or a pen's are the canvas's own.
func (s *surface) touchEvent(e js.Value) bool {
	if e.Get("pointerType").String() != "touch" {
		return false
	}
	t := &s.touch
	id := e.Get("pointerId").Int() + 1
	x, y := s.point(e)
	now := e.Get("timeStamp").Float()
	switch e.Get("type").String() {
	case "pointerdown":
		s.stopFling()
		if t.id != 0 {
			return true // a second finger: the first goes on alone
		}
		*t = touchState{id: id, startX: x, startY: y, lastX: x, lastY: y, last: now, holdFunc: t.holdFunc, flingFn: t.flingFn}
		t.hold = js.Global().Call("setTimeout", t.holdFunc, touchHold)
	case "pointermove":
		if id != t.id {
			return true
		}
		switch {
		case t.pressed:
			s.send(platform.SurfaceEvent{Kind: platform.PointerMove, X: x, Y: y})
		case t.scrolling || math.Hypot(x-t.startX, y-t.startY) > touchSlop:
			t.scrolling = true
			s.stopHold()
			dx, dy := x-t.lastX, y-t.lastY
			if dt := now - t.last; dt > 0 {
				// The speed, smoothed over the last moves.
				a := math.Min(1, dt/50)
				t.vx += (dx/dt - t.vx) * a
				t.vy += (dy/dt - t.vy) * a
			}
			s.send(platform.SurfaceEvent{Kind: platform.PointerScroll, X: t.startX, Y: t.startY, DX: -dx, DY: -dy, Precise: true})
		}
		t.lastX, t.lastY, t.last = x, y, now
	case "pointerup", "pointercancel":
		if id != t.id {
			return true
		}
		s.stopHold()
		t.id = 0
		canceled := e.Get("type").String() == "pointercancel"
		switch {
		case t.pressed:
			// Lifted, it opens the context menu there (Copy, for text
			// selected), as the secondary button does; the menu takes the
			// press, which clicks nothing then.
			s.downX, s.downY = x, y
			s.send(platform.SurfaceEvent{Kind: platform.PointerDown, X: x, Y: y, Button: 1, Clicks: 1})
			s.send(platform.SurfaceEvent{Kind: platform.PointerUp, X: x, Y: y, Button: 1})
			s.send(platform.SurfaceEvent{Kind: platform.PointerUp, X: x, Y: y})
			s.send(platform.SurfaceEvent{Kind: platform.PointerLeave})
		case t.scrolling:
			// A finger that stopped before it lifted throws nothing.
			if now-t.last > 80 || canceled {
				t.vx, t.vy = 0, 0
			}
			const most = 8 // DIPs a ms
			if v := math.Hypot(t.vx, t.vy); v > most {
				t.vx, t.vy = t.vx*most/v, t.vy*most/v
			}
			t.flingAt = now
			s.touchFling(now)
		case !canceled:
			s.touchPress(x, y, now)
			s.send(platform.SurfaceEvent{Kind: platform.PointerUp, X: x, Y: y})
			s.send(platform.SurfaceEvent{Kind: platform.PointerLeave})
		default:
			s.send(platform.SurfaceEvent{Kind: platform.PointerLeave})
		}
	}
	return true
}

// touchPress presses the content where a finger is at ms, as the mouse's
// main button.
func (s *surface) touchPress(x, y, ms float64) {
	s.downX, s.downY = x, y
	clicks := s.clicks.Press(0, x, y, uint32(ms), platform.DefaultDoubleClick)
	s.send(platform.SurfaceEvent{Kind: platform.PointerDown, X: x, Y: y, Clicks: clicks})
}

// touchFling scrolls on after a finger lifted, slowing down, until it
// stops or another finger touches.
func (s *surface) touchFling(now float64) {
	t := &s.touch
	dt := now - t.flingAt
	t.flingAt = now
	if dt > 0 {
		k := math.Pow(touchDecay, dt)
		// What the speed covers while it decays over dt.
		d := (1 - k) / -math.Log(touchDecay)
		dx, dy := t.vx*d, t.vy*d
		t.vx, t.vy = t.vx*k, t.vy*k
		if dx != 0 || dy != 0 {
			s.send(platform.SurfaceEvent{Kind: platform.PointerScroll, X: t.startX, Y: t.startY, DX: -dx, DY: -dy, Precise: true})
		}
	}
	if math.Hypot(t.vx, t.vy) < 0.02 {
		// A finger hovers nothing once it has gone.
		t.vx, t.vy = 0, 0
		s.send(platform.SurfaceEvent{Kind: platform.PointerLeave})
		return
	}
	t.fling = js.Global().Call("requestAnimationFrame", t.flingFn)
}

func (s *surface) stopHold() {
	if t := &s.touch; t.hold.Truthy() {
		js.Global().Call("clearTimeout", t.hold)
		t.hold = js.Undefined()
	}
}

func (s *surface) stopFling() {
	t := &s.touch
	if t.fling.Truthy() {
		js.Global().Call("cancelAnimationFrame", t.fling)
		t.fling = js.Undefined()
	}
	t.vx, t.vy = 0, 0
}

// listenViewport keeps a window filling the page (in body, or in a fixed
// #mygo) within the visual viewport on touch devices: the touch keyboard
// shrinks that, not the page, so content laid out to the page's bottom, as
// a text field there, would lie under the keyboard. Fitted to what shows,
// the field stays above the keyboard.
func (s *surface) listenViewport() {
	vv := js.Global().Get("visualViewport")
	if vv.IsUndefined() || vv.IsNull() {
		return
	}
	fit := func(js.Value) { s.fitViewport(vv) }
	s.on(vv, "resize", true, fit)
	s.on(vv, "scroll", true, fit)
	s.fitViewport(vv)
}

func (s *surface) fitViewport(vv js.Value) {
	el := s.w.el
	if p := el.Get("parentElement"); !p.Equal(js.Global().Get("document").Get("body")) {
		if js.Global().Call("getComputedStyle", p).Get("position").String() != "fixed" {
			return // laid out by the page
		}
		el = p
	}
	st := el.Get("style")
	if vv.Get("scale").Float() != 1 {
		// Zoomed in, the page is looked at in part: it keeps its layout.
		for _, k := range []string{"top", "left", "width", "height", "right", "bottom"} {
			st.Set(k, "")
		}
		return
	}
	// On whole device pixels, where the canvas's pixels are the screen's:
	// between them, the frames would be resampled, blurred.
	dpr := devicePixelRatio()
	px := func(v float64) string { return strconv.FormatFloat(math.Round(v*dpr)/dpr, 'f', -1, 64) + "px" }
	h := vv.Get("height").Float()
	st.Set("top", px(vv.Get("offsetTop").Float()))
	st.Set("left", px(vv.Get("offsetLeft").Float()))
	st.Set("width", px(vv.Get("width").Float()))
	st.Set("height", px(h))
	st.Set("right", "auto")
	st.Set("bottom", "auto")
	// data-mygo-keyboard marks it while the keyboard shows, for the page's
	// styles (the keyboard covers the home indicator's safe area).
	if js.Global().Get("document").Get("documentElement").Get("clientHeight").Float()-h > 100 {
		el.Call("setAttribute", "data-mygo-keyboard", "")
	} else {
		el.Call("removeAttribute", "data-mygo-keyboard")
	}
}
