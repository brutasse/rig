package ednlit

import (
	"fmt"
	"strconv"
	"strings"
)

func Format(v any) string {
	var b strings.Builder
	write(&b, v)
	return b.String()
}

func write(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("nil")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case int:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		s := strconv.FormatFloat(t, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		b.WriteString(s)
	case string:
		writeString(b, t)
	case Keyword:
		b.WriteString(t.String())
	case Symbol:
		b.WriteString(t.String())
	case Quoted:
		b.WriteByte('\'')
		write(b, t.V)
	case []any:
		writeSeq(b, "[", t, "]")
	case List:
		writeSeq(b, "(", []any(t), ")")
	case Set:
		b.WriteString("#{")
		for i, e := range t {
			if i > 0 {
				b.WriteByte(' ')
			}
			write(b, e)
		}
		b.WriteString("}")
	case Map:
		b.WriteByte('{')
		for i, p := range t {
			if i > 0 {
				b.WriteByte(' ')
			}
			write(b, p.K)
			b.WriteByte(' ')
			write(b, p.V)
		}
		b.WriteByte('}')
	default:
		b.WriteString(fmt.Sprintf("%v", t))
	}
}

func writeSeq(b *strings.Builder, open string, seq []any, close string) {
	b.WriteString(open)
	for i, e := range seq {
		if i > 0 {
			b.WriteByte(' ')
		}
		write(b, e)
	}
	b.WriteString(close)
}

func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
