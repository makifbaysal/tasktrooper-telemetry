package extract

import (
	"fmt"
	"os"
	"testing"
)

func TestDump(t *testing.T) {
	dir := os.Getenv("DUMP")
	if dir == "" {
		t.Skip()
	}
	r, err := Repo(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range r.Items {
		fmt.Printf("%-10s %-16s %-45q src=%-22q %s:%d fw=%s h=%s snip=%d\n", it.Kind, it.Service, it.Key, it.Source, it.File, it.Line, it.Framework, it.Handler, len(it.Snippet))
	}
	fmt.Println(r.Languages, r.Services)
}
