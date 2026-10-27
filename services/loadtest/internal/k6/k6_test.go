package k6

import (
	"context"
	"errors"
	"testing"
	"time"
)

const export = `{"metrics":{
 "http_req_duration":{"avg":120.5,"min":3,"med":100,"max":900,"p(90)":200,"p(95)":250,"p(99)":600,"thresholds":{"p(95)<500":false}},
 "http_reqs":{"count":1000,"rate":33.3},
 "http_req_failed":{"passes":20,"fails":980,"value":0.02,"thresholds":{"rate<0.01":true}},
 "vus_max":{"value":50,"min":50,"max":50}}}`

func TestParseSummary(t *testing.T) {
	s, failed, err := ParseSummary([]byte(export), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !failed || s.Requests != 1000 || s.P95 != 250 || s.ErrorRate != 0.02 || s.Errors != 20 || s.Vus != 50 || s.Rps != 33.3 {
		t.Fatalf("%+v failed=%v", s, failed)
	}
	if len(s.Thresholds) != 2 || s.Thresholds[0].Name != "http_req_duration p(95)<500" || !s.Thresholds[0].Passed || s.Thresholds[1].Passed {
		t.Fatalf("thresholds: %+v", s.Thresholds)
	}
}

func TestNotInstalled(t *testing.T) {
	_, err := Run(context.Background(), Options{Script: "x", Binary: "k6-definitely-not-installed"})
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v", err)
	}
}
