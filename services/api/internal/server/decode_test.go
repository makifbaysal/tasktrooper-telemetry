package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeOptional(t *testing.T) {
	type body struct {
		Title string `json:"title"`
	}
	tests := []struct {
		name          string
		body          string
		contentLength int64
		want          string
		wantErr       bool
	}{
		{name: "empty body, known length", body: "", contentLength: 0},
		{name: "empty body, unknown length (proxied)", body: "", contentLength: -1},
		{name: "valid body", body: `{"title":"x"}`, contentLength: -1, want: "x"},
		{name: "malformed body", body: `{"title":`, contentLength: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", strings.NewReader(tt.body))
			r.ContentLength = tt.contentLength
			var v body
			err := decodeOptional(r, &v)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if v.Title != tt.want {
				t.Fatalf("title = %q, want %q", v.Title, tt.want)
			}
		})
	}
}
