package enterprise

import (
	"bufio"
	"container/heap"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const eventBlockSize = 4096

func eventLess(a, b Event, order string) bool {
	at, bt := a.Occurred, b.Occurred
	if order == "delivery" {
		at, bt = a.Delivered, b.Delivered
	}
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.DeliveryID < b.DeliveryID
}

type mergeHead struct {
	event   Event
	decoder *json.Decoder
	file    *os.File
}
type eventHeap struct {
	heads []mergeHead
	order string
}

func (h eventHeap) Len() int           { return len(h.heads) }
func (h eventHeap) Less(i, j int) bool { return eventLess(h.heads[i].event, h.heads[j].event, h.order) }
func (h eventHeap) Swap(i, j int)      { h.heads[i], h.heads[j] = h.heads[j], h.heads[i] }
func (h *eventHeap) Push(x any)        { h.heads = append(h.heads, x.(mergeHead)) }
func (h *eventHeap) Pop() any {
	n := len(h.heads)
	x := h.heads[n-1]
	h.heads = h.heads[:n-1]
	return x
}

// Blocks are retained as inspectable dataset artifacts, never placed in /tmp.
// Merge at most 64 readers at once, so large datasets do not exhaust file descriptors.
func sortEventFile(input, dir, order string) error {
	var block []Event
	var paths []string
	flush := func() error {
		if len(block) == 0 {
			return nil
		}
		sort.Slice(block, func(i, j int) bool { return eventLess(block[i], block[j], order) })
		path := filepath.Join(dir, "blocks", fmt.Sprintf("%s-%06d.ndjson", order, len(paths)))
		w, e := newWriter(path)
		if e != nil {
			return e
		}
		for _, ev := range block {
			if e = w.e.Encode(ev); e != nil {
				_ = w.close()
				return e
			}
		}
		if e = w.close(); e != nil {
			return e
		}
		paths = append(paths, path)
		block = nil
		return nil
	}
	if e := readLines(input, func(ev Event, _ int) error {
		block = append(block, ev)
		if len(block) == eventBlockSize {
			return flush()
		}
		return nil
	}); e != nil {
		return e
	}
	if e := flush(); e != nil {
		return e
	}
	level := 0
	for len(paths) > 64 {
		var next []string
		for i := 0; i < len(paths); i += 64 {
			end := i + 64
			if end > len(paths) {
				end = len(paths)
			}
			path := filepath.Join(dir, "blocks", fmt.Sprintf("%s-merge-%d-%06d.ndjson", order, level, i))
			if e := mergeEvents(paths[i:end], path, order); e != nil {
				return e
			}
			next = append(next, path)
		}
		paths = next
		level++
	}
	return mergeEvents(paths, filepath.Join(dir, order+".ndjson"), order)
}
func mergeEvents(paths []string, out, order string) (err error) {
	h := &eventHeap{order: order}
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	for _, path := range paths {
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		files = append(files, f)
		decoder := json.NewDecoder(bufio.NewReaderSize(f, 16384))
		var ev Event
		if e = decoder.Decode(&ev); e == io.EOF {
			continue
		} else if e != nil {
			return e
		}
		heap.Push(h, mergeHead{ev, decoder, f})
	}
	w, e := newWriter(out)
	if e != nil {
		return e
	}
	defer func() {
		if e := w.close(); err == nil {
			err = e
		}
	}()
	for h.Len() > 0 {
		head := heap.Pop(h).(mergeHead)
		if e = w.e.Encode(head.event); e != nil {
			return e
		}
		head.event = Event{} // Decode must not retain keys from the previous Changes map.
		if e = head.decoder.Decode(&head.event); e == io.EOF {
			continue
		} else if e != nil {
			return e
		}
		heap.Push(h, head)
	}
	return nil
}
