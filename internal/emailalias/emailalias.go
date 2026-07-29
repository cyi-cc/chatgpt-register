package emailalias

import (
	"strconv"
	"strings"
)

func Base(address string) string {
	address = strings.TrimSpace(address)
	at := strings.LastIndex(address, "@")
	if at <= 0 {
		return address
	}
	local := address[:at]
	plus := strings.LastIndex(local, "+")
	if plus < 1 {
		return address
	}
	if _, err := strconv.Atoi(local[plus+1:]); err != nil {
		return address
	}
	return local[:plus] + address[at:]
}

func Address(base string, suffix string) string {
	base = strings.TrimSpace(base)
	at := strings.LastIndex(base, "@")
	if at <= 0 {
		return base
	}
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return base
	}
	return base[:at] + "+" + suffix + base[at:]
}

func LikePattern(base string) string {
	base = strings.TrimSpace(base)
	at := strings.LastIndex(base, "@")
	if at <= 0 {
		return ""
	}
	return escapeLike(base[:at]) + "+%" + escapeLike(base[at:])
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
