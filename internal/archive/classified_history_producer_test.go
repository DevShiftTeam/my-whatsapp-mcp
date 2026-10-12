package archive

import "testing"

func TestClassifiedHistoryPublicNoticeProducer(t *testing.T) {
	if !ClassifiedHistoryConflictProducerSupported("0e4dd9b2643ad3a4263333a4878f747190853c41") {
		t.Fatal("qualified public producer rejected")
	}
	for _, producer := range []string{"ff0a3bd06a7f921e50d9b5c6484c481066070db3", "7a7ca4a2ec0f619c0aff0a25839abcba68805a0a", "028633b7af17c13f016b4b0e95ad3769c3b8dcb1"} {
		if !ClassifiedHistoryConflictProducerSupported(producer) {
			t.Fatal("existing qualified producer rejected")
		}
	}
}
