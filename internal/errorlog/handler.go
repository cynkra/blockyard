package errorlog

import (
	"context"
	"log/slog"
)

// Handler wraps a delegate slog.Handler and additionally captures records
// whose level is >= Level into Store. The delegate handler is always
// consulted (and invoked if it enables the record) so the existing JSON
// stderr output keeps working unchanged.
//
// The delegate's own level filter determines what is written to stderr;
// Level controls what is captured into the ring buffer and may be more
// permissive.
type Handler struct {
	delegate slog.Handler
	store    *Store
	level    slog.Level
	attrs    []slog.Attr
	group    string // flattened group prefix for record attrs; "" for top level
}

// NewHandler returns a Handler that forwards to delegate and captures
// records at level >= captureLevel into store.
func NewHandler(delegate slog.Handler, store *Store, captureLevel slog.Level) *Handler {
	return &Handler{
		delegate: delegate,
		store:    store,
		level:    captureLevel,
	}
}

// Enabled reports whether either the delegate or the capture path wants
// the record. Without this, slog short-circuits WARN records when the
// delegate is set to ERROR, starving the ring buffer.
func (h *Handler) Enabled(ctx context.Context, lvl slog.Level) bool {
	if h.delegate != nil && h.delegate.Enabled(ctx, lvl) {
		return true
	}
	return lvl >= h.level
}

// Handle forwards the record to the delegate and, if the record meets
// the capture threshold, appends a flattened copy to the ring buffer.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if h.delegate != nil && h.delegate.Enabled(ctx, r.Level) {
		if err := h.delegate.Handle(ctx, r); err != nil {
			return err
		}
	}
	if r.Level < h.level || h.store == nil {
		return nil
	}
	e := Entry{
		Time:    r.Time,
		Level:   r.Level,
		Message: r.Message,
	}
	// h.attrs already carry their group prefix (baked in by WithAttrs),
	// so no extra prefix here.
	for _, a := range h.attrs {
		e.Attrs = appendAttr(e.Attrs, "", a)
	}
	// Record-time attrs still need the current group prefix.
	prefix := h.group
	r.Attrs(func(a slog.Attr) bool {
		e.Attrs = appendAttr(e.Attrs, prefix, a)
		return true
	})
	h.store.Append(e)
	return nil
}

// WithAttrs returns a new Handler whose captured records carry the
// supplied attrs in addition to whatever is passed at log time. When
// called inside a group, the group prefix is baked into the attr keys
// so later flattening at Handle time produces stable dotted names.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	captured := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		if h.group != "" && a.Key != "" {
			a.Key = h.group + "." + a.Key
		}
		captured[i] = a
	}
	newAttrs := make([]slog.Attr, 0, len(h.attrs)+len(captured))
	newAttrs = append(newAttrs, h.attrs...)
	newAttrs = append(newAttrs, captured...)
	return &Handler{
		delegate: withAttrs(h.delegate, attrs),
		store:    h.store,
		level:    h.level,
		attrs:    newAttrs,
		group:    h.group,
	}
}

// WithGroup returns a new Handler whose record attrs are prefixed with
// the given group name (joined with "." when nested). Group prefixing
// is applied at capture time so the flattened Attr keys remain
// distinguishable.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	group := name
	if h.group != "" {
		group = h.group + "." + name
	}
	return &Handler{
		delegate: withGroup(h.delegate, name),
		store:    h.store,
		level:    h.level,
		attrs:    h.attrs,
		group:    group,
	}
}

func withAttrs(h slog.Handler, attrs []slog.Attr) slog.Handler {
	if h == nil {
		return nil
	}
	return h.WithAttrs(attrs)
}

func withGroup(h slog.Handler, name string) slog.Handler {
	if h == nil {
		return nil
	}
	return h.WithGroup(name)
}

// appendAttr flattens a slog.Attr into the UI-ready Attr slice. Nested
// groups are expanded with dotted keys so e.g. `request.id` is visible.
func appendAttr(out []Attr, prefix string, a slog.Attr) []Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		groupPrefix := a.Key
		if prefix != "" && a.Key != "" {
			groupPrefix = prefix + "." + a.Key
		} else if a.Key == "" {
			groupPrefix = prefix
		}
		for _, inner := range v.Group() {
			out = appendAttr(out, groupPrefix, inner)
		}
		return out
	}
	key := a.Key
	if prefix != "" {
		if key == "" {
			key = prefix
		} else {
			key = prefix + "." + key
		}
	}
	return append(out, Attr{Key: key, Value: v.String()})
}
