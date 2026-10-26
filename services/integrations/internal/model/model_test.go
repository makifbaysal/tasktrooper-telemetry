package model

import "testing"

func TestSlug(t *testing.T) {
	tests := []struct{ provider, name, want string }{
		{"aws", "orders-db", "aws-orders-db"},
		{"aws", "sqs-eu-central-1", "aws-sqs-eu-central-1"},
		{"neon", "Shop/Main Branch", "neon-shop-main-branch"},
		{"gcp", "--x__y--", "gcp-x-y"},
	}
	for _, tt := range tests {
		if got := Slug(tt.provider, tt.name); got != tt.want {
			t.Errorf("Slug(%q,%q) = %q, want %q", tt.provider, tt.name, got, tt.want)
		}
	}
}
