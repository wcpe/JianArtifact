package formats

import (
	"reflect"
	"testing"
)

func TestParseNormalizesAndDeduplicates(t *testing.T) {
	got, err := Parse(" RAW, maven,raw ")
	if err != nil {
		t.Fatalf("Parse 返回错误：%v", err)
	}
	if want := []string{"maven", "raw"}; !reflect.DeepEqual(got.List(), want) {
		t.Fatalf("格式集合 = %#v，期望 %#v", got.List(), want)
	}
}

func TestParseEmptyDisablesAll(t *testing.T) {
	got, err := Parse("")
	if err != nil || got.Any() {
		t.Fatalf("空配置应得到空集合：set=%#v err=%v", got.List(), err)
	}
}

func TestParseRejectsUnknownAndEmptyItem(t *testing.T) {
	for _, raw := range []string{"raw,unknown", "raw,,maven"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("配置 %q 应拒绝", raw)
		}
	}
}
