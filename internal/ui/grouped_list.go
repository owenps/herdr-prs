package ui

import "github.com/owenps/herdr-prs/internal/prs"

// The list keeps item coordinates (cursor/top); this private layout owns the
// translation to terminal lines, including a context heading at any viewport top.
// Rows must already be grouped. Counts always describe the entire filtered group.
type groupedListLayout struct {
	groups []listGroup
	items  []listItem
}

type listGroup struct {
	name  prs.Group
	count int
}

type listItem struct {
	group, title int
}

type listBlock struct {
	item    int
	heading bool
	lines   int // one item row; never a heading without an item
}

func groupedList(rows []prs.PullRequest) groupedListLayout {
	l := groupedListLayout{items: make([]listItem, len(rows))}
	line := 0
	for i, p := range rows {
		if i == 0 || p.Group() != rows[i-1].Group() {
			l.groups = append(l.groups, listGroup{name: p.Group()})
			line++
		}
		group := len(l.groups) - 1
		l.groups[group].count++
		l.items[i] = listItem{group: group, title: line}
		line++
	}
	return l
}

func (l groupedListLayout) group(item int) listGroup {
	return l.groups[l.items[item].group]
}

// Height through the selected row, including the first context heading.
func (l groupedListLayout) height(top, cursor int) int {
	return l.items[cursor].title - l.items[top].title + 2
}

func (l groupedListLayout) keepVisible(top, cursor, height int) int {
	if len(l.items) == 0 {
		return 0
	}
	top = min(max(0, top), cursor)
	if height < 2 {
		// Selection beats headings on one-line terminals.
		return cursor
	}
	for top < cursor && l.height(top, cursor) > height {
		top++
	}
	// Backfill a short tail after expansion without disturbing a full viewport.
	for top > 0 && l.height(top-1, len(l.items)-1) <= height {
		top--
	}
	return top
}

func (l groupedListLayout) window(top, height int) []listBlock {
	var blocks []listBlock
	for i := top; i < len(l.items) && height > 0; i++ {
		heading := height > 0 && (i == top || l.items[i].group != l.items[i-1].group)
		// For a tiny viewport omit headings entirely. For larger viewports,
		// reserve at least a title after any heading, or stop before the group.
		if i == top && height < 2 {
			heading = false
		}
		if heading {
			if height < 2 {
				break
			}
			height--
		}
		lines := 1
		blocks = append(blocks, listBlock{item: i, heading: heading, lines: lines})
		height -= lines
	}
	return blocks
}

func (l groupedListLayout) page(cursor, direction, height int) int {
	if len(l.items) == 0 || height < 2 {
		return cursor + direction
	}
	count := 0
	if direction > 0 {
		for _, block := range l.window(cursor, height) {
			if block.lines == 1 {
				count++
			}
		}
	} else {
		count = cursor - l.keepVisible(0, cursor, height) + 1
	}
	return cursor + direction*max(1, count)
}
