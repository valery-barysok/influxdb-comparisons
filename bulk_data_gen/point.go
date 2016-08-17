package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/influxdata/influxdb-comparisons/mongo_serialization"
)

// Point wraps a single data point. It stores database-agnostic data
// representing one point in time of one measurement.
//
// Internally, Point uses byte slices instead of strings to try to minimize
// overhead.
type Point struct {
	MeasurementName []byte
	TagKeys         [][]byte
	TagValues       [][]byte
	FieldKeys       [][]byte
	FieldValues     []interface{}
	Timestamp       *time.Time
}

// Using these literals prevents the slices from escaping to the heap, saving
// a few micros per call:
var (
	charComma  = byte(',')
	charEquals = byte('=')
	charSpace  = byte(' ')
)

func (p *Point) Reset() {
	p.MeasurementName = nil
	p.TagKeys = p.TagKeys[:0]
	p.TagValues = p.TagValues[:0]
	p.FieldKeys = p.FieldKeys[:0]
	p.FieldValues = p.FieldValues[:0]
	p.Timestamp = nil
}

func (p *Point) SetTimestamp(t *time.Time) {
	p.Timestamp = t
}

func (p *Point) SetMeasurementName(s []byte) {
	p.MeasurementName = s
}

func (p *Point) AppendTag(key, value []byte) {
	p.TagKeys = append(p.TagKeys, key)
	p.TagValues = append(p.TagValues, value)
}

func (p *Point) AppendField(key []byte, value interface{}) {
	p.FieldKeys = append(p.FieldKeys, key)
	p.FieldValues = append(p.FieldValues, value)
}

// SerializeInfluxBulk writes Point data to the given writer, conforming to the
// InfluxDB wire protocol.
//
// This function writes output that looks like:
// <measurement>,<tag key>=<tag value> <field name>=<field value> <timestamp>\n
//
// For example:
// foo,tag0=bar baz=-1.0 100\n
//
// TODO(rw): Speed up this function. The bulk of time is spent in strconv.
func (p *Point) SerializeInfluxBulk(w io.Writer) (err error) {
	buf := make([]byte, 0, 256)
	buf = append(buf, p.MeasurementName...)

	for i := 0; i < len(p.TagKeys); i++ {
		buf = append(buf, charComma)
		buf = append(buf, p.TagKeys[i]...)
		buf = append(buf, charEquals)
		buf = append(buf, p.TagValues[i]...)
	}

	if len(p.FieldKeys) > 0 {
		buf = append(buf, charSpace)
	}

	for i := 0; i < len(p.FieldKeys); i++ {
		buf = append(buf, p.FieldKeys[i]...)
		buf = append(buf, charEquals)

		v := p.FieldValues[i]
		buf = fastFormatAppend(v, buf)

		// Influx uses 'i' to indicate integers:
		switch v.(type) {
		case int, int64:
			buf = append(buf, byte('i'))
		}

		if i+1 < len(p.FieldKeys) {
			buf = append(buf, charComma)
		}
	}

	buf = append(buf, []byte(fmt.Sprintf(" %d\n", p.Timestamp.UTC().UnixNano()))...)
	_, err = w.Write(buf)

	return err
}

// SerializeESBulk writes Point data to the given writer, conforming to the
// ElasticSearch bulk load protocol.
//
// This function writes output that looks like:
// <action line>
// <tags, fields, and timestamp>
//
// For example:
// { "create" : { "_index" : "measurement_otqio", "_type" : "point" } }\n
// { "tag_launx": "btkuw", "tag_gaijk": "jiypr", "field_wokxf": 0.08463898963964356, "field_zqstf": -0.043641533500086316, "timestamp": 171300 }\n
//
// TODO(rw): Speed up this function. The bulk of time is spent in strconv.
func (p *Point) SerializeESBulk(w io.Writer) error {
	action := "{ \"create\" : { \"_index\" : \"%s\", \"_type\" : \"point\" } }\n"
	_, err := fmt.Fprintf(w, action, p.MeasurementName)
	if err != nil {
		return err
	}

	buf := make([]byte, 0, 256)
	buf = append(buf, []byte("{")...)

	for i := 0; i < len(p.TagKeys); i++ {
		if i > 0 {
			buf = append(buf, []byte(", ")...)
		}
		buf = append(buf, []byte(fmt.Sprintf("\"%s\": ", p.TagKeys[i]))...)
		buf = append(buf, []byte(fmt.Sprintf("\"%s\"", p.TagValues[i]))...)
	}

	if len(p.TagKeys) > 0 && len(p.FieldKeys) > 0 {
		buf = append(buf, []byte(", ")...)
	}

	for i := 0; i < len(p.FieldKeys); i++ {
		if i > 0 {
			buf = append(buf, []byte(", ")...)
		}
		buf = append(buf, "\""...)
		buf = append(buf, p.FieldKeys[i]...)
		buf = append(buf, "\": "...)

		v := p.FieldValues[i]
		buf = fastFormatAppend(v, buf)
	}

	if len(p.TagKeys) > 0 || len(p.FieldKeys) > 0 {
		buf = append(buf, []byte(", ")...)
	}
	// Timestamps in ES must be millisecond precision:
	buf = append(buf, []byte(fmt.Sprintf("\"timestamp\": %d }\n", p.Timestamp.UTC().UnixNano()/1e6))...)

	_, err = w.Write(buf)
	if err != nil {
		return err
	}

	return nil
}

// SerializeCassandra writes Point data to the given writer, conforming to the
// Cassandra query format.
//
// This function writes output that looks like:
// INSERT INTO <tablename> (series_id, ts_ns, value) VALUES (<series_id>, <timestamp_nanoseconds>, <field value>)
// where series_id looks like: <measurement>,<tagset>#<field name>#<time shard>
//
// For example:
// INSERT INTO all_series (series_id, timestamp_ns, value) VALUES ('cpu,hostname=host_01#user#2016-01-01', 12345, 42.1)\n
func (p *Point) SerializeCassandra(w io.Writer) (err error) {
	seriesIdPrefix := make([]byte, 0, 256)
	seriesIdPrefix = append(seriesIdPrefix, p.MeasurementName...)
	for i := 0; i < len(p.TagKeys); i++ {
		seriesIdPrefix = append(seriesIdPrefix, charComma)
		seriesIdPrefix = append(seriesIdPrefix, p.TagKeys[i]...)
		seriesIdPrefix = append(seriesIdPrefix, charEquals)
		seriesIdPrefix = append(seriesIdPrefix, p.TagValues[i]...)
	}

	timestampNanos := p.Timestamp.UTC().UnixNano()
	timestampBucket := p.Timestamp.UTC().Format("2006-01-02")

	for fieldId := 0; fieldId < len(p.FieldKeys); fieldId++ {
		v := p.FieldValues[fieldId]
		tableName := fmt.Sprintf("measurements.series_%s", typeNameForCassandra(v))

		buf := make([]byte, 0, 256)
		buf = append(buf, []byte("INSERT INTO ")...)
		buf = append(buf, []byte(tableName)...)
		buf = append(buf, []byte(" (series_id, timestamp_ns, value) VALUES ('")...)
		buf = append(buf, seriesIdPrefix...)
		buf = append(buf, byte('#'))
		buf = append(buf, p.FieldKeys[fieldId]...)
		buf = append(buf, byte('#'))
		buf = append(buf, []byte(timestampBucket)...)
		buf = append(buf, byte('\''))
		buf = append(buf, charComma)
		buf = append(buf, charSpace)
		buf = append(buf, []byte(fmt.Sprintf("%d, ", timestampNanos))...)

		buf = fastFormatAppend(v, buf)

		buf = append(buf, []byte(")\n")...)

		_, err := w.Write(buf)
		if err != nil {
			return err
		}
	}

	return nil
}

// SerializeMongo writes Point data to the given writer, conforming to the
// mongo_serialization FlatBuffers format.
func (p *Point) SerializeMongo(w io.Writer) (err error) {
	// Prepare the series id prefix, which is the set of tags associated
	// with this point. The series id prefix is the base of each value's
	// particular collection name:
	seriesId := bufPool.Get().([]byte)
	seriesId = append(seriesId, p.MeasurementName...)
	for i := 0; i < len(p.TagKeys); i++ {
		seriesId = append(seriesId, charComma)
		seriesId = append(seriesId, p.TagKeys[i]...)
		seriesId = append(seriesId, charEquals)
		seriesId = append(seriesId, p.TagValues[i]...)
	}
	seriesIdPrefixLen := len(seriesId)

	// Prepare the timestamp, which is the same for each value in this
	// Point:
	timestampNanos := p.Timestamp.UTC().UnixNano()

	// Fetch a flatbuffers builder from a pool:
	lenBuf := bufPool8.Get().([]byte)
	builder := fbBuilderPool.Get().(*flatbuffers.Builder)

	// For each field in this Point, serialize its:
	// collection name (series id prefix + the name of the value)
	// timestamp in nanos (int64)
	// numeric value (int, int64, or float64 -- determined by reflection)
	for fieldId := 0; fieldId < len(p.FieldKeys); fieldId++ {
		fieldName := p.FieldKeys[fieldId]
		genericValue := p.FieldValues[fieldId]

		// Make the collection name for this value, taking care to
		// reuse the seriesId slice:
		seriesId = seriesId[:seriesIdPrefixLen]
		seriesId = append(seriesId, '#')
		seriesId = append(seriesId, fieldName...)

		// build the flatbuffer representing this point:
		builder.Reset()
		seriesIdOffset := builder.CreateByteVector(seriesId)
		mongo_serialization.ItemStart(builder)
		mongo_serialization.ItemAddSeriesId(builder, seriesIdOffset)
		mongo_serialization.ItemAddTimestampNanos(builder, timestampNanos)

		switch v := genericValue.(type) {
		// (We can't switch on groups of types (e.g. int,int64) because
		// that does not make v concrete.)
		case int, int64:
			mongo_serialization.ItemAddValueType(builder, mongo_serialization.ValueTypeLong)
			switch v2 := v.(type) {
			case int:
				mongo_serialization.ItemAddLongValue(builder, int64(v2))
			case int64:
				mongo_serialization.ItemAddLongValue(builder, v2)
			}
		case float64:
			mongo_serialization.ItemAddValueType(builder, mongo_serialization.ValueTypeDouble)
			mongo_serialization.ItemAddDoubleValue(builder, v)
		default:
			panic(fmt.Sprintf("logic error in mongo serialization, %s", reflect.TypeOf(v)))
		}
		mongo_serialization.ItemAddSeriesId(builder, seriesIdOffset)
		rootTable := mongo_serialization.ItemEnd(builder)
		builder.Finish(rootTable)

		// Access the finished byte slice representing this flatbuffer:
		buf := builder.FinishedBytes()

		// Write the metadata for the flatbuffer object:
		binary.LittleEndian.PutUint64(lenBuf, uint64(len(buf)))
		_, err = w.Write(lenBuf)
		if err != nil {
			return err
		}

		// Write the flatbuffer object:
		_, err := w.Write(buf)
		if err != nil {
			return err
		}
	}

	// Give the flatbuffers builder back to a pool:
	builder.Reset()
	fbBuilderPool.Put(builder)

	// Give the series id byte slice back to a pool:
	seriesId = seriesId[:0]
	bufPool.Put(seriesId)

	// Give the 8-byte buf back to a pool:
	bufPool8.Put(lenBuf)

	return nil
}

func typeNameForCassandra(v interface{}) string {
	switch v.(type) {
	case int, int64:
		return "bigint"
	case float64:
		return "double"
	case float32:
		return "float"
	case bool:
		return "boolean"
	case []byte, string:
		return "blob"
	default:
		panic(fmt.Sprintf("unknown field type for %#v", v))
	}
}

func fastFormatAppend(v interface{}, buf []byte) []byte {
	switch v.(type) {
	case int:
		return strconv.AppendInt(buf, int64(v.(int)), 10)
	case int64:
		return strconv.AppendInt(buf, v.(int64), 10)
	case float64:
		return strconv.AppendFloat(buf, v.(float64), 'f', 16, 64)
	case float32:
		return strconv.AppendFloat(buf, float64(v.(float32)), 'f', 16, 32)
	case bool:
		return strconv.AppendBool(buf, v.(bool))
	case []byte:
		buf = append(buf, v.([]byte)...)
		return buf
	case string:
		buf = append(buf, v.(string)...)
		return buf
	default:
		panic(fmt.Sprintf("unknown field type for %#v", v))
	}
}
