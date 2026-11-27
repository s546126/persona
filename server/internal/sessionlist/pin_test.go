package sessionlist

import (
	"reflect"
	"testing"
)

func pinned(v Item, at int) Item {
	v.Fold = &Fold{Pin: at}
	return v
}

func TestPinnedItemsSortFirstInPinOrder(t *testing.T) {
	var i Index
	i.Replace("batch", []Item{testItem("new", 30), testItem("mid", 20), pinned(testItem("old", 10), 2), pinned(testItem("oldest", 0), 1)})
	if got := sessionIDs(mustQuery(t, &i, Options{})); !reflect.DeepEqual(got, []string{"oldest", "old", "new", "mid"}) {
		t.Fatalf("order = %v", got)
	}
	// a pinned item crosses the page boundary onto page 1
	if got := sessionIDs(mustQuery(t, &i, Options{Limit: 1})); !reflect.DeepEqual(got, []string{"oldest"}) {
		t.Fatalf("page 1 = %v", got)
	}
	// a pin change is a change of the row: a delta carries it, and the full list follows
	base := i.Revision()
	i.Replace("batch", []Item{testItem("new", 30), testItem("mid", 20), testItem("old", 10), pinned(testItem("oldest", 0), 1)})
	delta := mustQuery(t, &i, Options{Since: base})
	if got := sessionIDs(delta); !reflect.DeepEqual(got, []string{"old"}) {
		t.Fatalf("delta = %v", got)
	}
	if got := sessionIDs(mustQuery(t, &i, Options{})); !reflect.DeepEqual(got, []string{"oldest", "new", "mid", "old"}) {
		t.Fatalf("after unpin = %v", got)
	}
}

func TestUnpinnedRowsKeepTheirOrder(t *testing.T) {
	var i Index
	// a Fold without Pin (a runtime row's hierarchy) sorts as before
	folded := testItem("folded", 5)
	folded.Fold = &Fold{Todo: "todo-1"}
	i.Replace("batch", []Item{testItem("c", 0), folded, testItem("a", 10)})
	if got := sessionIDs(mustQuery(t, &i, Options{})); !reflect.DeepEqual(got, []string{"a", "folded", "c"}) {
		t.Fatalf("order = %v", got)
	}
}
