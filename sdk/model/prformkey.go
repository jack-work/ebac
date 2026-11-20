package model

import "strings"

// PrFormKey makes a PR key safe as one path segment.
func PrFormKey(k PRKey) string { return strings.ReplaceAll(string(k), ".", "%2E") }
