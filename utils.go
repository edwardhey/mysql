// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2012 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Registry for custom tls.Configs
var (
	tlsConfigLock     sync.RWMutex
	tlsConfigRegistry map[string]*tls.Config
	errNilPtr         = errors.New("destination pointer is nil") // embedded in descriptive error
)

// RegisterTLSConfig registers a custom tls.Config to be used with sql.Open.
// Use the key as a value in the DSN where tls=value.
//
// Note: The provided tls.Config is exclusively owned by the driver after
// registering it.
//
//	rootCertPool := x509.NewCertPool()
//	pem, err := os.ReadFile("/path/ca-cert.pem")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	if ok := rootCertPool.AppendCertsFromPEM(pem); !ok {
//	    log.Fatal("Failed to append PEM.")
//	}
//	clientCert := make([]tls.Certificate, 0, 1)
//	certs, err := tls.LoadX509KeyPair("/path/client-cert.pem", "/path/client-key.pem")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	clientCert = append(clientCert, certs)
//	mysql.RegisterTLSConfig("custom", &tls.Config{
//	    RootCAs: rootCertPool,
//	    Certificates: clientCert,
//	})
//	db, err := sql.Open("mysql", "user@tcp(localhost:3306)/test?tls=custom")
func RegisterTLSConfig(key string, config *tls.Config) error {
	if _, isBool := readBool(key); isBool || strings.ToLower(key) == "skip-verify" || strings.ToLower(key) == "preferred" {
		return fmt.Errorf("key '%s' is reserved", key)
	}

	tlsConfigLock.Lock()
	if tlsConfigRegistry == nil {
		tlsConfigRegistry = make(map[string]*tls.Config)
	}

	tlsConfigRegistry[key] = config
	tlsConfigLock.Unlock()
	return nil
}

// DeregisterTLSConfig removes the tls.Config associated with key.
func DeregisterTLSConfig(key string) {
	tlsConfigLock.Lock()
	if tlsConfigRegistry != nil {
		delete(tlsConfigRegistry, key)
	}
	tlsConfigLock.Unlock()
}

func getTLSConfigClone(key string) (config *tls.Config) {
	tlsConfigLock.RLock()
	if v, ok := tlsConfigRegistry[key]; ok {
		config = v.Clone()
	}
	tlsConfigLock.RUnlock()
	return
}

// Returns the bool value of the input.
// The 2nd return value indicates if the input was a valid bool value
func readBool(input string) (value bool, valid bool) {
	switch input {
	case "1", "true", "TRUE", "True":
		return true, true
	case "0", "false", "FALSE", "False":
		return false, true
	}

	// Not a valid bool value
	return
}

/******************************************************************************
*                           Time related utils                                *
******************************************************************************/

func parseDateTime(b []byte, loc *time.Location) (time.Time, error) {
	const base = "0000-00-00 00:00:00.000000"
	switch len(b) {
	case 10, 19, 21, 22, 23, 24, 25, 26: // up to "YYYY-MM-DD HH:MM:SS.MMMMMM"
		if string(b) == base[:len(b)] {
			return time.Time{}, nil
		}

		year, err := parseByteYear(b)
		if err != nil {
			return time.Time{}, err
		}
		if b[4] != '-' {
			return time.Time{}, fmt.Errorf("bad value for field: `%c`", b[4])
		}

		m, err := parseByte2Digits(b[5], b[6])
		if err != nil {
			return time.Time{}, err
		}
		month := time.Month(m)

		if b[7] != '-' {
			return time.Time{}, fmt.Errorf("bad value for field: `%c`", b[7])
		}

		day, err := parseByte2Digits(b[8], b[9])
		if err != nil {
			return time.Time{}, err
		}
		if len(b) == 10 {
			return time.Date(year, month, day, 0, 0, 0, 0, loc), nil
		}

		if b[10] != ' ' {
			return time.Time{}, fmt.Errorf("bad value for field: `%c`", b[10])
		}

		hour, err := parseByte2Digits(b[11], b[12])
		if err != nil {
			return time.Time{}, err
		}
		if b[13] != ':' {
			return time.Time{}, fmt.Errorf("bad value for field: `%c`", b[13])
		}

		min, err := parseByte2Digits(b[14], b[15])
		if err != nil {
			return time.Time{}, err
		}
		if b[16] != ':' {
			return time.Time{}, fmt.Errorf("bad value for field: `%c`", b[16])
		}

		sec, err := parseByte2Digits(b[17], b[18])
		if err != nil {
			return time.Time{}, err
		}
		if len(b) == 19 {
			return time.Date(year, month, day, hour, min, sec, 0, loc), nil
		}

		if b[19] != '.' {
			return time.Time{}, fmt.Errorf("bad value for field: `%c`", b[19])
		}
		nsec, err := parseByteNanoSec(b[20:])
		if err != nil {
			return time.Time{}, err
		}
		return time.Date(year, month, day, hour, min, sec, nsec, loc), nil
	default:
		return time.Time{}, fmt.Errorf("invalid time bytes: %s", b)
	}
}

func parseByteYear(b []byte) (int, error) {
	year, n := 0, 1000
	for i := 0; i < 4; i++ {
		v, err := bToi(b[i])
		if err != nil {
			return 0, err
		}
		year += v * n
		n /= 10
	}
	return year, nil
}

func parseByte2Digits(b1, b2 byte) (int, error) {
	d1, err := bToi(b1)
	if err != nil {
		return 0, err
	}
	d2, err := bToi(b2)
	if err != nil {
		return 0, err
	}
	return d1*10 + d2, nil
}

func parseByteNanoSec(b []byte) (int, error) {
	ns, digit := 0, 100000 // max is 6-digits
	for i := 0; i < len(b); i++ {
		v, err := bToi(b[i])
		if err != nil {
			return 0, err
		}
		ns += v * digit
		digit /= 10
	}
	// nanoseconds has 10-digits. (needs to scale digits)
	// 10 - 6 = 4, so we have to multiple 1000.
	return ns * 1000, nil
}

func bToi(b byte) (int, error) {
	if b < '0' || b > '9' {
		return 0, errors.New("not [0-9]")
	}
	return int(b - '0'), nil
}

func parseBinaryDateTime(num uint64, data []byte, loc *time.Location) (driver.Value, error) {
	switch num {
	case 0:
		return time.Time{}, nil
	case 4:
		return time.Date(
			int(binary.LittleEndian.Uint16(data[:2])), // year
			time.Month(data[2]),                       // month
			int(data[3]),                              // day
			0, 0, 0, 0,
			loc,
		), nil
	case 7:
		return time.Date(
			int(binary.LittleEndian.Uint16(data[:2])), // year
			time.Month(data[2]),                       // month
			int(data[3]),                              // day
			int(data[4]),                              // hour
			int(data[5]),                              // minutes
			int(data[6]),                              // seconds
			0,
			loc,
		), nil
	case 11:
		return time.Date(
			int(binary.LittleEndian.Uint16(data[:2])), // year
			time.Month(data[2]),                       // month
			int(data[3]),                              // day
			int(data[4]),                              // hour
			int(data[5]),                              // minutes
			int(data[6]),                              // seconds
			int(binary.LittleEndian.Uint32(data[7:11]))*1000, // nanoseconds
			loc,
		), nil
	}
	return nil, fmt.Errorf("invalid DATETIME packet length %d", num)
}

func appendDateTime(buf []byte, t time.Time, timeTruncate time.Duration) ([]byte, error) {
	if timeTruncate > 0 {
		t = t.Truncate(timeTruncate)
	}

	year, month, day := t.Date()
	hour, min, sec := t.Clock()
	nsec := t.Nanosecond()

	if year < 1 || year > 9999 {
		return buf, errors.New("year is not in the range [1, 9999]: " + strconv.Itoa(year)) // use errors.New instead of fmt.Errorf to avoid year escape to heap
	}
	year100 := year / 100
	year1 := year % 100

	var localBuf [len("2006-01-02T15:04:05.999999999")]byte // does not escape
	localBuf[0], localBuf[1], localBuf[2], localBuf[3] = digits10[year100], digits01[year100], digits10[year1], digits01[year1]
	localBuf[4] = '-'
	localBuf[5], localBuf[6] = digits10[month], digits01[month]
	localBuf[7] = '-'
	localBuf[8], localBuf[9] = digits10[day], digits01[day]

	if hour == 0 && min == 0 && sec == 0 && nsec == 0 {
		return append(buf, localBuf[:10]...), nil
	}

	localBuf[10] = ' '
	localBuf[11], localBuf[12] = digits10[hour], digits01[hour]
	localBuf[13] = ':'
	localBuf[14], localBuf[15] = digits10[min], digits01[min]
	localBuf[16] = ':'
	localBuf[17], localBuf[18] = digits10[sec], digits01[sec]

	if nsec == 0 {
		return append(buf, localBuf[:19]...), nil
	}
	nsec100000000 := nsec / 100000000
	nsec1000000 := (nsec / 1000000) % 100
	nsec10000 := (nsec / 10000) % 100
	nsec100 := (nsec / 100) % 100
	nsec1 := nsec % 100
	localBuf[19] = '.'

	// milli second
	localBuf[20], localBuf[21], localBuf[22] =
		digits01[nsec100000000], digits10[nsec1000000], digits01[nsec1000000]
	// micro second
	localBuf[23], localBuf[24], localBuf[25] =
		digits10[nsec10000], digits01[nsec10000], digits10[nsec100]
	// nano second
	localBuf[26], localBuf[27], localBuf[28] =
		digits01[nsec100], digits10[nsec1], digits01[nsec1]

	// trim trailing zeros
	n := len(localBuf)
	for n > 0 && localBuf[n-1] == '0' {
		n--
	}

	return append(buf, localBuf[:n]...), nil
}

// zeroDateTime is used in formatBinaryDateTime to avoid an allocation
// if the DATE or DATETIME has the zero value.
// It must never be changed.
// The current behavior depends on database/sql copying the result.
var zeroDateTime = []byte("0000-00-00 00:00:00.000000")

const digits01 = "0123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789"
const digits10 = "0000000000111111111122222222223333333333444444444455555555556666666666777777777788888888889999999999"

func appendMicrosecs(dst, src []byte, decimals int) []byte {
	if decimals <= 0 {
		return dst
	}
	if len(src) == 0 {
		return append(dst, ".000000"[:decimals+1]...)
	}

	microsecs := binary.LittleEndian.Uint32(src[:4])
	p1 := byte(microsecs / 10000)
	microsecs -= 10000 * uint32(p1)
	p2 := byte(microsecs / 100)
	microsecs -= 100 * uint32(p2)
	p3 := byte(microsecs)

	switch decimals {
	default:
		return append(dst, '.',
			digits10[p1], digits01[p1],
			digits10[p2], digits01[p2],
			digits10[p3], digits01[p3],
		)
	case 1:
		return append(dst, '.',
			digits10[p1],
		)
	case 2:
		return append(dst, '.',
			digits10[p1], digits01[p1],
		)
	case 3:
		return append(dst, '.',
			digits10[p1], digits01[p1],
			digits10[p2],
		)
	case 4:
		return append(dst, '.',
			digits10[p1], digits01[p1],
			digits10[p2], digits01[p2],
		)
	case 5:
		return append(dst, '.',
			digits10[p1], digits01[p1],
			digits10[p2], digits01[p2],
			digits10[p3],
		)
	}
}

func formatBinaryDateTime(src []byte, length uint8) (driver.Value, error) {
	// length expects the deterministic length of the zero value,
	// negative time and 100+ hours are automatically added if needed
	if len(src) == 0 {
		return zeroDateTime[:length], nil
	}
	var dst []byte      // return value
	var p1, p2, p3 byte // current digit pair

	switch length {
	case 10, 19, 21, 22, 23, 24, 25, 26:
	default:
		t := "DATE"
		if length > 10 {
			t += "TIME"
		}
		return nil, fmt.Errorf("illegal %s length %d", t, length)
	}
	switch len(src) {
	case 4, 7, 11:
	default:
		t := "DATE"
		if length > 10 {
			t += "TIME"
		}
		return nil, fmt.Errorf("illegal %s packet length %d", t, len(src))
	}
	dst = make([]byte, 0, length)
	// start with the date
	year := binary.LittleEndian.Uint16(src[:2])
	pt := year / 100
	p1 = byte(year - 100*uint16(pt))
	p2, p3 = src[2], src[3]
	dst = append(dst,
		digits10[pt], digits01[pt],
		digits10[p1], digits01[p1], '-',
		digits10[p2], digits01[p2], '-',
		digits10[p3], digits01[p3],
	)
	if length == 10 {
		return dst, nil
	}
	if len(src) == 4 {
		return append(dst, zeroDateTime[10:length]...), nil
	}
	dst = append(dst, ' ')
	p1 = src[4] // hour
	src = src[5:]

	// p1 is 2-digit hour, src is after hour
	p2, p3 = src[0], src[1]
	dst = append(dst,
		digits10[p1], digits01[p1], ':',
		digits10[p2], digits01[p2], ':',
		digits10[p3], digits01[p3],
	)
	return appendMicrosecs(dst, src[2:], int(length)-20), nil
}

func formatBinaryTime(src []byte, length uint8) (driver.Value, error) {
	// length expects the deterministic length of the zero value,
	// negative time and 100+ hours are automatically added if needed
	if len(src) == 0 {
		return zeroDateTime[11 : 11+length], nil
	}
	var dst []byte // return value

	switch length {
	case
		8,                      // time (can be up to 10 when negative and 100+ hours)
		10, 11, 12, 13, 14, 15: // time with fractional seconds
	default:
		return nil, fmt.Errorf("illegal TIME length %d", length)
	}
	switch len(src) {
	case 8, 12:
	default:
		return nil, fmt.Errorf("invalid TIME packet length %d", len(src))
	}
	// +2 to enable negative time and 100+ hours
	dst = make([]byte, 0, length+2)
	if src[0] == 1 {
		dst = append(dst, '-')
	}
	days := binary.LittleEndian.Uint32(src[1:5])
	hours := int64(days)*24 + int64(src[5])

	if hours >= 100 {
		dst = strconv.AppendInt(dst, hours, 10)
	} else {
		dst = append(dst, digits10[hours], digits01[hours])
	}

	min, sec := src[6], src[7]
	dst = append(dst, ':',
		digits10[min], digits01[min], ':',
		digits10[sec], digits01[sec],
	)
	return appendMicrosecs(dst, src[8:], int(length)-9), nil
}

/******************************************************************************
*                       Convert from and to bytes                             *
******************************************************************************/

func uint64ToBytes(n uint64) []byte {
	return []byte{
		byte(n),
		byte(n >> 8),
		byte(n >> 16),
		byte(n >> 24),
		byte(n >> 32),
		byte(n >> 40),
		byte(n >> 48),
		byte(n >> 56),
	}
}

func uint64ToString(n uint64) []byte {
	var a [20]byte
	i := 20

	// U+0030 = 0
	// ...
	// U+0039 = 9

	var q uint64
	for n >= 10 {
		i--
		q = n / 10
		a[i] = uint8(n-q*10) + 0x30
		n = q
	}

	i--
	a[i] = uint8(n) + 0x30

	return a[i:]
}

// treats string value as unsigned integer representation
func stringToInt(b []byte) int {
	val := 0
	for i := range b {
		val *= 10
		val += int(b[i] - 0x30)
	}
	return val
}

// returns the string read as a bytes slice, whether the value is NULL,
// the number of bytes read and an error, in case the string is longer than
// the input slice
func readLengthEncodedString(b []byte) ([]byte, bool, int, error) {
	// Get length
	num, isNull, n := readLengthEncodedInteger(b)
	if num < 1 {
		return b[n:n], isNull, n, nil
	}

	n += int(num)

	// Check data length
	if len(b) >= n {
		return b[n-int(num) : n : n], false, n, nil
	}
	return nil, false, n, io.EOF
}

// returns the number of bytes skipped and an error, in case the string is
// longer than the input slice
func skipLengthEncodedString(b []byte) (int, error) {
	// Get length
	num, _, n := readLengthEncodedInteger(b)
	if num < 1 {
		return n, nil
	}

	n += int(num)

	// Check data length
	if len(b) >= n {
		return n, nil
	}
	return n, io.EOF
}

// returns the number read, whether the value is NULL and the number of bytes read
func readLengthEncodedInteger(b []byte) (uint64, bool, int) {
	// See issue #349
	if len(b) == 0 {
		return 0, true, 1
	}

	switch b[0] {
	// 251: NULL
	case 0xfb:
		return 0, true, 1

	// 252: value of following 2
	case 0xfc:
		return uint64(b[1]) | uint64(b[2])<<8, false, 3

	// 253: value of following 3
	case 0xfd:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, false, 4

	// 254: value of following 8
	case 0xfe:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16 |
				uint64(b[4])<<24 | uint64(b[5])<<32 | uint64(b[6])<<40 |
				uint64(b[7])<<48 | uint64(b[8])<<56,
			false, 9
	}

	// 0-250: value of first byte
	return uint64(b[0]), false, 1
}

// encodes a uint64 value and appends it to the given bytes slice
func appendLengthEncodedInteger(b []byte, n uint64) []byte {
	switch {
	case n <= 250:
		return append(b, byte(n))

	case n <= 0xffff:
		return append(b, 0xfc, byte(n), byte(n>>8))

	case n <= 0xffffff:
		return append(b, 0xfd, byte(n), byte(n>>8), byte(n>>16))
	}
	return append(b, 0xfe, byte(n), byte(n>>8), byte(n>>16), byte(n>>24),
		byte(n>>32), byte(n>>40), byte(n>>48), byte(n>>56))
}

func appendLengthEncodedString(b []byte, s string) []byte {
	b = appendLengthEncodedInteger(b, uint64(len(s)))
	return append(b, s...)
}

// reserveBuffer checks cap(buf) and expand buffer to len(buf) + appendSize.
// If cap(buf) is not enough, reallocate new buffer.
func reserveBuffer(buf []byte, appendSize int) []byte {
	newSize := len(buf) + appendSize
	if cap(buf) < newSize {
		// Grow buffer exponentially
		newBuf := make([]byte, len(buf)*2+appendSize)
		copy(newBuf, buf)
		buf = newBuf
	}
	return buf[:newSize]
}

// escapeBytesBackslash escapes []byte with backslashes (\)
// This escapes the contents of a string (provided as []byte) by adding backslashes before special
// characters, and turning others into specific escape sequences, such as
// turning newlines into \n and null bytes into \0.
// https://github.com/mysql/mysql-server/blob/mysql-5.7.5/mysys/charset.c#L823-L932
func escapeBytesBackslash(buf, v []byte) []byte {
	pos := len(buf)
	buf = reserveBuffer(buf, len(v)*2)

	for _, c := range v {
		switch c {
		case '\x00':
			buf[pos+1] = '0'
			buf[pos] = '\\'
			pos += 2
		case '\n':
			buf[pos+1] = 'n'
			buf[pos] = '\\'
			pos += 2
		case '\r':
			buf[pos+1] = 'r'
			buf[pos] = '\\'
			pos += 2
		case '\x1a':
			buf[pos+1] = 'Z'
			buf[pos] = '\\'
			pos += 2
		case '\'':
			buf[pos+1] = '\''
			buf[pos] = '\\'
			pos += 2
		case '"':
			buf[pos+1] = '"'
			buf[pos] = '\\'
			pos += 2
		case '\\':
			buf[pos+1] = '\\'
			buf[pos] = '\\'
			pos += 2
		default:
			buf[pos] = c
			pos++
		}
	}

	return buf[:pos]
}

// escapeStringBackslash is similar to escapeBytesBackslash but for string.
func escapeStringBackslash(buf []byte, v string) []byte {
	pos := len(buf)
	buf = reserveBuffer(buf, len(v)*2)

	for i := 0; i < len(v); i++ {
		c := v[i]
		switch c {
		case '\x00':
			buf[pos+1] = '0'
			buf[pos] = '\\'
			pos += 2
		case '\n':
			buf[pos+1] = 'n'
			buf[pos] = '\\'
			pos += 2
		case '\r':
			buf[pos+1] = 'r'
			buf[pos] = '\\'
			pos += 2
		case '\x1a':
			buf[pos+1] = 'Z'
			buf[pos] = '\\'
			pos += 2
		case '\'':
			buf[pos+1] = '\''
			buf[pos] = '\\'
			pos += 2
		case '"':
			buf[pos+1] = '"'
			buf[pos] = '\\'
			pos += 2
		case '\\':
			buf[pos+1] = '\\'
			buf[pos] = '\\'
			pos += 2
		default:
			buf[pos] = c
			pos++
		}
	}

	return buf[:pos]
}

// escapeBytesQuotes escapes apostrophes in []byte by doubling them up.
// This escapes the contents of a string by doubling up any apostrophes that
// it contains. This is used when the NO_BACKSLASH_ESCAPES SQL_MODE is in
// effect on the server.
// https://github.com/mysql/mysql-server/blob/mysql-5.7.5/mysys/charset.c#L963-L1038
func escapeBytesQuotes(buf, v []byte) []byte {
	pos := len(buf)
	buf = reserveBuffer(buf, len(v)*2)

	for _, c := range v {
		if c == '\'' {
			buf[pos+1] = '\''
			buf[pos] = '\''
			pos += 2
		} else {
			buf[pos] = c
			pos++
		}
	}

	return buf[:pos]
}

// escapeStringQuotes is similar to escapeBytesQuotes but for string.
func escapeStringQuotes(buf []byte, v string) []byte {
	pos := len(buf)
	buf = reserveBuffer(buf, len(v)*2)

	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '\'' {
			buf[pos+1] = '\''
			buf[pos] = '\''
			pos += 2
		} else {
			buf[pos] = c
			pos++
		}
	}

	return buf[:pos]
}

/******************************************************************************
*                               Sync utils                                    *
******************************************************************************/

// noCopy may be embedded into structs which must not be copied
// after the first use.
//
// See https://github.com/golang/go/issues/8005#issuecomment-190753527
// for details.
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock() {}

// Unlock is a no-op used by -copylocks checker from `go vet`.
// noCopy should implement sync.Locker from Go 1.11
// https://github.com/golang/go/commit/c2eba53e7f80df21d51285879d51ab81bcfbf6bc
// https://github.com/golang/go/issues/26165
func (*noCopy) Unlock() {}

// atomicError is a wrapper for atomically accessed error values
type atomicError struct {
	_     noCopy
	value atomic.Value
}

// Set sets the error value regardless of the previous value.
// The value must not be nil
func (ae *atomicError) Set(value error) {
	ae.value.Store(value)
}

// Value returns the current error value
func (ae *atomicError) Value() error {
	if v := ae.value.Load(); v != nil {
		// this will panic if the value doesn't implement the error interface
		return v.(error)
	}
	return nil
}

func namedValueToValue(named []driver.NamedValue) ([]driver.Value, error) {
	dargs := make([]driver.Value, len(named))
	for n, param := range named {
		if len(param.Name) > 0 {
			// TODO: support the use of Named Parameters #561
			return nil, errors.New("mysql: driver does not support the use of Named Parameters")
		}
		dargs[n] = param.Value
	}
	return dargs, nil
}

func mapIsolationLevel(level driver.IsolationLevel) (string, error) {
	switch sql.IsolationLevel(level) {
	case sql.LevelRepeatableRead:
		return "REPEATABLE READ", nil
	case sql.LevelReadCommitted:
		return "READ COMMITTED", nil
	case sql.LevelReadUncommitted:
		return "READ UNCOMMITTED", nil
	case sql.LevelSerializable:
		return "SERIALIZABLE", nil
	default:
		return "", fmt.Errorf("mysql: unsupported isolation level: %v", level)
	}
}

type RawBytes []byte

func convertAssignRows(dest, src interface{}) error {
	// Common cases, without reflect.
	switch s := src.(type) {
	case string:
		switch d := dest.(type) {
		case *string:
			if d == nil {
				return errNilPtr
			}
			*d = s
			return nil
		case *[]byte:
			if d == nil {
				return errNilPtr
			}
			*d = []byte(s)
			return nil
		case *RawBytes:
			if d == nil {
				return errNilPtr
			}
			*d = append((*d)[:0], s...)
			return nil
		}
	case []byte:
		switch d := dest.(type) {
		case *string:
			if d == nil {
				return errNilPtr
			}
			*d = string(s)
			return nil
		case *interface{}:
			if d == nil {
				return errNilPtr
			}
			*d = cloneBytes(s)
			return nil
		case *[]byte:
			if d == nil {
				return errNilPtr
			}
			*d = cloneBytes(s)
			return nil
		case *RawBytes:
			if d == nil {
				return errNilPtr
			}
			*d = s
			return nil
		}
	case time.Time:
		switch d := dest.(type) {
		case *time.Time:
			*d = s
			return nil
		case *string:
			*d = s.Format(time.RFC3339Nano)
			return nil
		case *[]byte:
			if d == nil {
				return errNilPtr
			}
			*d = []byte(s.Format(time.RFC3339Nano))
			return nil
		case *RawBytes:
			if d == nil {
				return errNilPtr
			}
			*d = s.AppendFormat((*d)[:0], time.RFC3339Nano)
			return nil
		}
	case decimalDecompose:
		switch d := dest.(type) {
		case decimalCompose:
			return d.Compose(s.Decompose(nil))
		}
	case nil:
		switch d := dest.(type) {
		case *interface{}:
			if d == nil {
				return errNilPtr
			}
			*d = nil
			return nil
		case *[]byte:
			if d == nil {
				return errNilPtr
			}
			*d = nil
			return nil
		case *RawBytes:
			if d == nil {
				return errNilPtr
			}
			*d = nil
			return nil
		}
	}

	if scanner, ok := dest.(Scanner); ok {
		return scanner.Scan(src)
	}

	var sv reflect.Value

	switch d := dest.(type) {
	case *string:
		sv = reflect.ValueOf(src)
		switch sv.Kind() {
		case reflect.Bool,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			*d = asString(src)
			return nil
		}
	case *[]byte:
		sv = reflect.ValueOf(src)
		if b, ok := asBytes(nil, sv); ok {
			*d = b
			return nil
		}
	case *RawBytes:
		sv = reflect.ValueOf(src)
		if b, ok := asBytes([]byte(*d)[:0], sv); ok {
			*d = RawBytes(b)
			return nil
		}
	case *bool:
		bv, err := driver.Bool.ConvertValue(src)
		if err == nil {
			*d = bv.(bool)
		}
		return err
	case *interface{}:
		*d = src
		return nil
	}

	dpv := reflect.ValueOf(dest)
	if dpv.Kind() != reflect.Ptr {
		return errors.New("destination not a pointer")
	}
	if dpv.IsNil() {
		return errNilPtr
	}

	if !sv.IsValid() {
		sv = reflect.ValueOf(src)
	}

	dv := reflect.Indirect(dpv)
	if sv.IsValid() && sv.Type().AssignableTo(dv.Type()) {
		switch b := src.(type) {
		case []byte:
			dv.Set(reflect.ValueOf(cloneBytes(b)))
		default:
			dv.Set(sv)
		}
		return nil
	}

	if dv.Kind() == sv.Kind() && sv.Type().ConvertibleTo(dv.Type()) {
		dv.Set(sv.Convert(dv.Type()))
		return nil
	}

	// The following conversions use a string value as an intermediate representation
	// to convert between various numeric types.
	//
	// This also allows scanning into user defined types such as "type Int int64".
	// For symmetry, also check for string destination types.
	switch dv.Kind() {
	case reflect.Ptr:
		if src == nil {
			dv.Set(reflect.Zero(dv.Type()))
			return nil
		}
		dv.Set(reflect.New(dv.Type().Elem()))
		return convertAssignRows(dv.Interface(), src)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if src == nil {
			return fmt.Errorf("converting NULL to %s is unsupported", dv.Kind())
		}
		s := asString(src)
		i64, err := strconv.ParseInt(s, 10, dv.Type().Bits())
		if err != nil {
			err = strconvErr(err)
			return fmt.Errorf("converting driver.Value type %T (%q) to a %s: %v", src, s, dv.Kind(), err)
		}
		dv.SetInt(i64)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if src == nil {
			return fmt.Errorf("converting NULL to %s is unsupported", dv.Kind())
		}
		s := asString(src)
		u64, err := strconv.ParseUint(s, 10, dv.Type().Bits())
		if err != nil {
			err = strconvErr(err)
			return fmt.Errorf("converting driver.Value type %T (%q) to a %s: %v", src, s, dv.Kind(), err)
		}
		dv.SetUint(u64)
		return nil
	case reflect.Float32, reflect.Float64:
		if src == nil {
			return fmt.Errorf("converting NULL to %s is unsupported", dv.Kind())
		}
		s := asString(src)
		f64, err := strconv.ParseFloat(s, dv.Type().Bits())
		if err != nil {
			err = strconvErr(err)
			return fmt.Errorf("converting driver.Value type %T (%q) to a %s: %v", src, s, dv.Kind(), err)
		}
		dv.SetFloat(f64)
		return nil
	case reflect.String:
		if src == nil {
			return fmt.Errorf("converting NULL to %s is unsupported", dv.Kind())
		}
		switch v := src.(type) {
		case string:
			dv.SetString(v)
			return nil
		case []byte:
			dv.SetString(string(v))
			return nil
		}
	}

	return fmt.Errorf("unsupported Scan, storing driver.Value type %T into type %T", src, dest)
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

type decimal interface {
	decimalDecompose
	decimalCompose
}

type decimalDecompose interface {
	// Decompose returns the internal decimal state in parts.
	// If the provided buf has sufficient capacity, buf may be returned as the coefficient with
	// the value set and length set as appropriate.
	Decompose(buf []byte) (form byte, negative bool, coefficient []byte, exponent int32)
}

type decimalCompose interface {
	// Compose sets the internal decimal value from parts. If the value cannot be
	// represented then an error should be returned.
	Compose(form byte, negative bool, coefficient []byte, exponent int32) error
}

// Scanner is an interface used by Scan.
type Scanner interface {
	// Scan assigns a value from a database driver.
	//
	// The src value will be of one of the following types:
	//
	//    int64
	//    float64
	//    bool
	//    []byte
	//    string
	//    time.Time
	//    nil - for NULL values
	//
	// An error should be returned if the value cannot be stored
	// without loss of information.
	//
	// Reference types such as []byte are only valid until the next call to Scan
	// and should not be retained. Their underlying memory is owned by the driver.
	// If retention is necessary, copy their values before the next call to Scan.
	Scan(src interface{}) error
}

func asString(src interface{}) string {
	switch v := src.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	rv := reflect.ValueOf(src)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64)
	case reflect.Float32:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 32)
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool())
	}
	return fmt.Sprintf("%v", src)
}

func asBytes(buf []byte, rv reflect.Value) (b []byte, ok bool) {
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(buf, rv.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.AppendUint(buf, rv.Uint(), 10), true
	case reflect.Float32:
		return strconv.AppendFloat(buf, rv.Float(), 'g', -1, 32), true
	case reflect.Float64:
		return strconv.AppendFloat(buf, rv.Float(), 'g', -1, 64), true
	case reflect.Bool:
		return strconv.AppendBool(buf, rv.Bool()), true
	case reflect.String:
		s := rv.String()
		return append(buf, s...), true
	}
	return
}

func strconvErr(err error) error {
	if ne, ok := err.(*strconv.NumError); ok {
		return ne.Err
	}
	return err
}
