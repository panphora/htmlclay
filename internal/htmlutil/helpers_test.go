package htmlutil

import (
	"reflect"
	"strings"
	"testing"
)

func TestReadHelperNamesAttributeSyntaxAndCase(t *testing.T) {
	data := []byte(`<!doctype html><HTML><HEAD>
		<META NAME = "htmlclay-helper" CONTENT = "first-helper">
		<meta name='htmlclay-helper' content='second'>
		<meta name=htmlclay-helper content=third>
		<meta CONTENT = fourth NAME = htmlclay-helper>
	</HEAD><body></body></HTML>`)
	want := []string{"first-helper", "second", "third", "fourth"}
	if got := ReadHelperNames(data); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadHelperNames = %v, want %v", got, want)
	}
}

func TestReadHelperNamesDecodesCharacterReferencesAndKeepsFirstDuplicateAttribute(t *testing.T) {
	data := []byte(`<head>
		<meta name="htmlclay-helper" content="searc&#104;">
		<meta name="htmlclay-helper" name="other" content="format">
	</head>`)
	want := []string{"search", "format"}
	if got := ReadHelperNames(data); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadHelperNames = %v, want %v", got, want)
	}
}

func TestReadHelperNamesIgnoresNonDocumentMarkup(t *testing.T) {
	data := []byte(`<head>
		<!-- <meta name="htmlclay-helper" content="commented"> -->
		<script>const tag = '<meta name="htmlclay-helper" content="scripted">'</script>
		<style>.x::after { content: '<meta name="htmlclay-helper" content="styled">'; }</style>
		<template></head><body><meta name="htmlclay-helper" content="templated"><template><meta name="htmlclay-helper" content="nested"></template></template>
		<meta name="htmlclay-helper" content="real">
	</head>`)
	if got := ReadHelperNames(data); !reflect.DeepEqual(got, []string{"real"}) {
		t.Fatalf("ReadHelperNames = %v, want [real]", got)
	}
}

func TestReadHelperNamesStopsAtHeadBoundaries(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"end head", `<head></head><meta name="htmlclay-helper" content="late">`},
		{"body", `<head><body><meta name="htmlclay-helper" content="late">`},
		{"implicit body", `<head><div></div><meta name="htmlclay-helper" content="late">`},
		{"non-whitespace body text", `<head>body text<meta name="htmlclay-helper" content="late">`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadHelperNames([]byte(tc.data)); len(got) != 0 {
				t.Fatalf("ReadHelperNames = %v, want none", got)
			}
		})
	}
}

func TestReadHelperNamesDoesNotTreatQuotedBoundaryAsMarkup(t *testing.T) {
	data := []byte(`<head><meta data-example="</head><body" name="htmlclay-helper" content="search"></head>`)
	if got := ReadHelperNames(data); !reflect.DeepEqual(got, []string{"search"}) {
		t.Fatalf("ReadHelperNames = %v, want [search]", got)
	}
}

func TestReadHelperNamesRejectsMalformedFinalTag(t *testing.T) {
	data := []byte(`<head><meta name="htmlclay-helper" content="search"`)
	if got := ReadHelperNames(data); len(got) != 0 {
		t.Fatalf("ReadHelperNames = %v, want none", got)
	}
}

func TestReadHelperNamesDeduplicatesSkipsInvalidAndCaps(t *testing.T) {
	var data strings.Builder
	data.WriteString("<head>")
	data.WriteString(`<meta name="htmlclay-helper" content="first">`)
	data.WriteString(`<meta name="htmlclay-helper" content="first">`)
	data.WriteString(`<meta name="htmlclay-helper" content="Not-Valid">`)
	for _, name := range []string{"helper2", "helper3", "helper4", "helper5", "helper6", "helper7", "helper8", "helper9", "helper10"} {
		data.WriteString(`<meta name="htmlclay-helper" content="`)
		data.WriteString(name)
		data.WriteString(`">`)
	}
	want := []string{"first", "helper2", "helper3", "helper4", "helper5", "helper6", "helper7", "helper8"}
	if got := ReadHelperNames([]byte(data.String())); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadHelperNames = %v, want %v", got, want)
	}
}

func TestReadHelperNamesFindsDeclarationPast64KiB(t *testing.T) {
	css := strings.Repeat(".ordinary{color:black}", 3200)
	data := []byte(`<head><style>` + css + `</style><meta name="htmlclay-helper" content="search"></head>`)
	if declarationAt := strings.Index(string(data), `<meta`); declarationAt <= 64<<10 || declarationAt >= helperScanLimit {
		t.Fatalf("fixture declaration offset = %d", declarationAt)
	}
	if got := ReadHelperNames(data); !reflect.DeepEqual(got, []string{"search"}) {
		t.Fatalf("ReadHelperNames = %v, want [search]", got)
	}
}

func TestReadHelperNamesStopsAt512KiB(t *testing.T) {
	data := []byte("<head>" + strings.Repeat(" ", helperScanLimit) + `<meta name="htmlclay-helper" content="search">`)
	if got := ReadHelperNames(data); len(got) != 0 {
		t.Fatalf("ReadHelperNames = %v, want none", got)
	}
}
