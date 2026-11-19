package gcp

import (
	"google.golang.org/api/bigquery/v2"
	"strings"
	"testing"
)

func TestBigQueryJobFailureOmitsReflectedPrivateValues(t *testing.T) {
	err := jobError([]*bigquery.ErrorProto{{Reason: "accessDenied", Message: "opaque-test-value 1111222233334444 synthetic@example.test"}})
	if err == nil || !strings.Contains(err.Error(), "reason=accessDenied") {
		t.Fatal("lost safe job context")
	}
	for _, value := range []string{"opaque-test-value", "1111222233334444", "synthetic@example.test"} {
		if strings.Contains(err.Error(), value) {
			t.Fatal("BigQuery failure leaked private response")
		}
	}
}
