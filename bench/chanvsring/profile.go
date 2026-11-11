package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// cpuByLabel sums the CPU time of a runtime/pprof CPU profile (gzipped protobuf, profile.proto) by the value of one
// label key; samples without that label sum under "". It decodes only the fields it needs: Profile.sample (2),
// Profile.string_table (6), Sample.value (2), Sample.label (3), Label.key (1) and Label.str (2).
func cpuByLabel(profile []byte, key string) (map[string]time.Duration, error) {
	reader, err := gzip.NewReader(bytes.NewReader(profile))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	var strings []string
	type sample struct {
		nanos  int64
		labels map[int64]int64 // key index to string index
	}
	var samples []sample
	err = fields(data, func(number int, value uint64, bytes []byte) error {
		switch number {
		case 6:
			strings = append(strings, string(bytes))
		case 2:
			each := sample{labels: map[int64]int64{}}
			var values []int64
			err := fields(bytes, func(number int, value uint64, bytes []byte) error {
				switch {
				case number == 2 && bytes != nil: // packed
					for len(bytes) > 0 {
						decoded, size := binary.Uvarint(bytes)
						if size <= 0 {
							return errors.New("profile: bad packed value")
						}
						values, bytes = append(values, int64(decoded)), bytes[size:]
					}
				case number == 2:
					values = append(values, int64(value))
				case number == 3:
					var labelKey, labelString int64
					err := fields(bytes, func(number int, value uint64, _ []byte) error {
						switch number {
						case 1:
							labelKey = int64(value)
						case 2:
							labelString = int64(value)
						}
						return nil
					})
					each.labels[labelKey] = labelString
					return err
				}
				return nil
			})
			if len(values) > 0 {
				each.nanos = values[len(values)-1] // a CPU profile's values are [samples, cpu nanoseconds]
			}
			samples = append(samples, each)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	keyIndex := int64(-1)
	for index, each := range strings {
		if each == key {
			keyIndex = int64(index)
		}
	}
	split := map[string]time.Duration{}
	for _, each := range samples {
		name := ""
		if index, found := each.labels[keyIndex]; found && index >= 0 && index < int64(len(strings)) {
			name = strings[index]
		}
		split[name] += time.Duration(each.nanos)
	}
	return split, nil
}

// fields calls visit for each field of one protobuf message: varints with their value, length-delimited fields with
// their bytes (nil for varints). Fixed-width fields are skipped.
func fields(data []byte, visit func(number int, value uint64, bytes []byte) error) error {
	for len(data) > 0 {
		tag, size := binary.Uvarint(data)
		if size <= 0 {
			return errors.New("profile: bad tag")
		}
		data = data[size:]
		number, wireType := int(tag>>3), tag&7
		var value uint64
		var bytes []byte
		switch wireType {
		case 0:
			if value, size = binary.Uvarint(data); size <= 0 {
				return errors.New("profile: bad varint")
			}
			data = data[size:]
		case 1, 5:
			width := map[uint64]int{1: 8, 5: 4}[wireType]
			if len(data) < width {
				return errors.New("profile: short fixed field")
			}
			data = data[width:]
			continue
		case 2:
			length, size := binary.Uvarint(data)
			if size <= 0 || uint64(len(data)-size) < length {
				return errors.New("profile: bad length")
			}
			bytes, data = data[size:size+int(length)], data[size+int(length):]
		default:
			return errors.New("profile: unknown wire type")
		}
		if err := visit(number, value, bytes); err != nil {
			return err
		}
	}
	return nil
}
