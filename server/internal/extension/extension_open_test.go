package extension

import (
	"slices"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/i18n"
)

// A full extension page may open an existing Persona session or to-do; a card may not, and other targets stay stripped.
func TestFullPageKeepsSessionAndTaskOpen(t *testing.T) {
	page, ok := Slot("nav_page")
	if !ok || page.Size != SizeFull {
		t.Fatalf("nav_page slot: %+v", page)
	}
	card, ok := Slot("overview_card")
	if !ok || card.Size != SizeCompact {
		t.Fatalf("overview_card slot: %+v", card)
	}
	view := func(target string) *View {
		return &View{Sections: []Section{{Items: []UIElem{
			{Type: "text", Text: "before"},
			{Type: "button", Title: "go", Action: &UIAction{ID: "go", Open: target}},
		}}}}
	}

	for _, name := range []string{"nav_page", "extension_page", "plugin_page"} {
		page, ok := Slot(name)
		if !ok || !FullPage(name) {
			t.Fatalf("%s slot: %+v", name, page)
		}
		v := view("persona://session/machine-1/session-1")
		if _, err := Fit(v, page, UIVersion); err != nil {
			t.Fatal(err)
		}
		if a := v.Sections[0].Items[1].Action; a == nil || a.Open == "" {
			t.Fatalf("%s lost its session open", name)
		}
	}

	v := view("persona://task/todo-1")
	if _, err := Fit(v, page, UIVersion); err != nil {
		t.Fatal(err)
	}
	if a := v.Sections[0].Items[1].Action; a == nil || a.Open == "" {
		t.Fatal("nav_page lost its task open")
	}

	v = view("persona://session/machine-1/session-1")
	if _, err := Fit(v, card, UIVersion); err != nil {
		t.Fatal(err)
	}
	if len(v.Sections[0].Items) != 1 {
		t.Fatalf("a card kept a session-open button: %#v", v.Sections[0].Items)
	}

	for _, name := range []string{"launcher_section", "machine_section", "inbox_item"} {
		section, ok := Slot(name)
		if !ok || section.Size != SizeFull || FullPage(name) {
			t.Fatalf("%s slot: %+v", name, section)
		}
		v = view("persona://session/machine-1/session-1")
		if _, err := Fit(v, section, UIVersion); err != nil {
			t.Fatal(err)
		}
		if len(v.Sections[0].Items) != 1 {
			t.Fatalf("%s kept a page-only session-open button: %#v", name, v.Sections[0].Items)
		}
	}

	v = view("https://example.com/one")
	if _, err := Fit(v, page, UIVersion); err != nil {
		t.Fatal(err)
	}
	if len(v.Sections[0].Items) != 1 {
		t.Fatalf("an https open survived on a page: %#v", v.Sections[0].Items)
	}
}

func TestTerminalAndExtTerminalOpenTargets(t *testing.T) {
	view := func(target string) *View {
		return &View{Sections: []Section{{Items: []UIElem{
			{Type: "text", Text: "before"},
			{Type: "button", Title: "open", Action: &UIAction{ID: "open", Open: target}},
		}}}}
	}

	// Valid persona://terminal/<machine_id> kept only on full extension pages.
	for _, name := range []string{"nav_page", "extension_page", "plugin_page"} {
		slot, ok := Slot(name)
		if !ok {
			t.Fatalf("missing slot %s", name)
		}
		v := view("persona://terminal/mac-studio-1")
		if _, err := FitForExtension(v, slot, UIVersion, "ensemble", nil); err != nil {
			t.Fatalf("%s FitForExtension: %v", name, err)
		}
		if len(v.Sections[0].Items) != 2 || v.Sections[0].Items[1].Action == nil || v.Sections[0].Items[1].Action.Open != "persona://terminal/mac-studio-1" {
			t.Fatalf("%s lost valid persona://terminal open: %#v", name, v.Sections[0].Items)
		}
	}

	// Stripped on overview_card, capsule_card, inbox_item, and *_section slots.
	for _, name := range []string{"overview_card", "capsule_card", "inbox_item", "launcher_section", "machine_section"} {
		slot, ok := Slot(name)
		if !ok {
			t.Fatalf("missing slot %s", name)
		}
		for _, target := range []string{"persona://terminal/mac-studio-1", "persona://ext-terminal/ensemble/shell-1"} {
			v := view(target)
			if _, err := FitForExtension(v, slot, UIVersion, "ensemble", nil); err != nil {
				t.Fatalf("%s FitForExtension(%s): %v", name, target, err)
			}
			if len(v.Sections[0].Items) != 1 {
				t.Fatalf("%s failed to strip %s: %#v", name, target, v.Sections[0].Items)
			}
		}
	}

	// Reject malformed persona://terminal and persona://ext-terminal targets.
	badTargets := []string{
		"persona://terminal/",
		"persona://terminal/..",
		"persona://terminal/.",
		"persona://terminal/a/b",
		"persona://terminal/a%2Fb",
		"persona://terminal/mac%201",
		"persona://terminal/mac-1?x=1",
		"persona://terminal/mac-1#frag",
		"persona://terminal/mac\x01",
		"persona://ext-terminal/ensemble",
		"persona://ext-terminal/ensemble/a/b",
		"persona://ext-terminal/ensemble/..",
		"persona://ext-terminal/BadUpper/shell-1",
		"persona://ext-terminal/ensemble/shell%201",
		"persona://ext-terminal/ensemble/shell-1?x=1",
	}
	navPage, _ := Slot("nav_page")
	for _, bad := range badTargets {
		if err := wire.CheckExtensionOpen(bad); err == nil {
			t.Fatalf("CheckExtensionOpen(%q) unexpectedly accepted", bad)
		}
		v := view(bad)
		if _, err := FitForExtension(v, navPage, UIVersion, "ensemble", nil); err != nil {
			t.Fatalf("FitForExtension(%q): %v", bad, err)
		}
		if len(v.Sections[0].Items) != 1 {
			t.Fatalf("malformed target %q survived FitForExtension: %#v", bad, v.Sections[0].Items)
		}
	}

	// Cross-extension spoofing: persona://ext-terminal/<other_ext>/<target> must be stripped,
	// while matching ext_id is kept on full pages.
	own := view("persona://ext-terminal/ensemble/worker-1")
	if _, err := FitForExtension(own, navPage, UIVersion, "ensemble", nil); err != nil {
		t.Fatalf("own ext-terminal: %v", err)
	}
	if len(own.Sections[0].Items) != 2 || own.Sections[0].Items[1].Action.Open != "persona://ext-terminal/ensemble/worker-1" {
		t.Fatalf("own ext-terminal was stripped: %#v", own.Sections[0].Items)
	}
	spoofed := view("persona://ext-terminal/other-ext/worker-1")
	if _, err := FitForExtension(spoofed, navPage, UIVersion, "ensemble", nil); err != nil {
		t.Fatalf("spoofed ext-terminal: %v", err)
	}
	if len(spoofed.Sections[0].Items) != 1 {
		t.Fatalf("cross-extension ext-terminal survived FitForExtension: %#v", spoofed.Sections[0].Items)
	}
}

func TestMachinesExecScopeSafetyAndI18n(t *testing.T) {
	if !slices.Contains(HubScopes, ScopeMachinesExec) {
		t.Fatalf("HubScopes missing %s", ScopeMachinesExec)
	}
	if InEveryScope(ScopeMachinesExec) {
		t.Fatalf("security redline violated: InEveryScope(%s) must be false", ScopeMachinesExec)
	}
	for _, lang := range []i18n.Lang{i18n.En, i18n.ZhHans, i18n.Ja} {
		title, ok := ScopeTitleIn(lang, ScopeMachinesExec)
		if !ok || title == "" || title == ScopeMachinesExec {
			t.Fatalf("missing localized title for %s in %s: ok=%v got %q", ScopeMachinesExec, lang, ok, title)
		}
	}
}
