// documentation:
// https://dev.mysql.com/doc/internals/en/rows-event.html
// https://dev.mysql.com/doc/internals/en/com-query-response.html#packet-Protocol::ColumnType
package mysql

import (
	"bytes"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"time"
)

type RowsEvent struct {
	header                EventHeader
	tableId               uint64
	tableMap              *TableMapEvent
	flags                 uint16
	columnsPresentBitmap1 Bitfield
	columnsPresentBitmap2 Bitfield
	rows                  []map[string]driver.Value
}

func (parser *eventParser) parseRowsEvent(buf *bytes.Buffer) (event *RowsEvent, err error) {
	var columnCount uint64

	event = new(RowsEvent)
	err = binary.Read(buf, binary.LittleEndian, &event.header)

	headerSize := parser.format.eventTypeHeaderLengths[event.header.EventType-1]
	var tableIdSize int
	if headerSize == 6 {
		tableIdSize = 4
	} else {
		tableIdSize = 6
	}
	event.tableId, err = readFixedLengthInteger(buf, tableIdSize)

	err = binary.Read(buf, binary.LittleEndian, &event.flags)
	columnCount, _, err = readLengthEncodedInt(buf)

	event.columnsPresentBitmap1 = Bitfield(buf.Next(int((columnCount + 7) / 8)))
	switch event.header.EventType {
	case UPDATE_ROWS_EVENTv1, UPDATE_ROWS_EVENTv2:
		event.columnsPresentBitmap2 = Bitfield(buf.Next(int((columnCount + 7) / 8)))
	}

	event.tableMap = parser.tableMap[event.tableId]
	for buf.Len() > 0 {
		var row map[string]driver.Value
		row, err = parseEventRow(buf, event.tableMap, parser.tableSchemaMap[event.tableId])
		if err != nil {
			return
		}

		event.rows = append(event.rows, row)
	}

	return
}

func parseEventRow(buf *bytes.Buffer, tableMap *TableMapEvent, tableSchemaMap []*column_schema_type) (row map[string]driver.Value, e error) {
	columnsCount := len(tableMap.columnTypes)
	row = make(map[string]driver.Value)
	bitfieldSize := (columnsCount + 7) / 8
	nullBitMap := Bitfield(buf.Next(bitfieldSize))
	for i := 0; i < columnsCount; i++ {
		column_name := tableSchemaMap[i].COLUMN_NAME
		if nullBitMap.isSet(uint(i)) {
			row[column_name] = nil
			continue
		}

		switch tableMap.columnMetaData[i].column_type {
		case FIELD_TYPE_NULL:
			row[column_name] = nil

		case FIELD_TYPE_TINY:
			var b byte
			b, e = buf.ReadByte()
			if tableSchemaMap[i].is_bool == true{
				switch int(b) {
				case 1:row[column_name]= true
				case 0:row[column_name]=false
				default: row[column_name] = int(b)
				}
			}else{
				row[column_name] = int(b)
			}

		case FIELD_TYPE_SHORT:
			var short int16
			e = binary.Read(buf, binary.LittleEndian, &short)
			row[column_name] = int64(short)

		case FIELD_TYPE_YEAR:
			var b byte
			b, e = buf.ReadByte()
			if e == nil && b != 0 {
				//time.Date(int(b)+1900, time.January, 0, 0, 0, 0, 0, time.UTC)
				row[column_name] = strconv.Itoa(int(b) + 1900)
			}

		case FIELD_TYPE_INT24:
			row[column_name], e = readFixedLengthInteger(buf, 3)

		case FIELD_TYPE_LONG:
			var long int32
			e = binary.Read(buf, binary.LittleEndian, &long)
			row[column_name] = int64(long)

		case FIELD_TYPE_LONGLONG:
			var longlong int64
			e = binary.Read(buf, binary.LittleEndian, &longlong)
			row[column_name] = longlong

		case FIELD_TYPE_FLOAT:
			var float float32
			e = binary.Read(buf, binary.LittleEndian, &float)
			row[column_name] = float64(float)

		case FIELD_TYPE_DOUBLE:
			var double float64
			e = binary.Read(buf, binary.LittleEndian, &double)
			row[column_name] = double

		case FIELD_TYPE_DECIMAL:
			return nil, fmt.Errorf("parseEventRow unimplemented for field type %s", fieldTypeName(tableMap.columnTypes[i]))
		case FIELD_TYPE_NEWDECIMAL:
			digits_per_integer := 9
			compressed_bytes := [10]int{0, 1, 1, 2, 2, 3, 3, 4, 4, 4}
			integral := (tableMap.columnMetaData[i].precision - tableMap.columnMetaData[i].decimals)
			uncomp_integral := int(int(integral) / digits_per_integer)
			uncomp_fractional := int(int(tableMap.columnMetaData[i].decimals) / digits_per_integer)
			comp_integral := integral - (uncomp_integral * digits_per_integer)
			comp_fractional := tableMap.columnMetaData[i].decimals - (uncomp_fractional * digits_per_integer)

			var value int
			var res string
			var mask int
			var size int
			size = compressed_bytes[comp_integral]

			bufPaket := &paket{
				buf:     buf,
				buydata: make([]byte, 0),
			}
			b := bufPaket.readByte()

			if int(b)&128 != 0 {
				res = ""
				mask = 0
			} else {
				mask = -1
				res = "-"
			}

			var tmp *bytes.Buffer = new(bytes.Buffer)
			binary.Write(tmp, binary.LittleEndian, uint8(b)^128)
			bufPaket.unread(tmp.Next(1))

			if size > 0 {
				d := bufPaket.read(size)
				data := bytes.NewBuffer(d)

				var v1 int32
				binary.Read(data, binary.BigEndian, &v1)
				value = int(v1) ^ mask
				res += strconv.Itoa(value)
			}

			for i := 0; i < uncomp_integral; i++ {
				value = int(read_uint64_be_by_bytes(bufPaket.read(4))) ^ mask
				res += fmt.Sprintf("%09d", value)
			}

			res += "."

			for i := 0; i < uncomp_integral; i++ {
				value = int(read_uint64_be_by_bytes(bufPaket.read(4))) ^ mask
				res += fmt.Sprintf("%09d", value)
			}
			size = compressed_bytes[comp_fractional]
			if size > 0 {
				value = int(read_uint64_be_by_bytes(bufPaket.read(size))) ^ mask
				res += fmt.Sprintf("%0*d", comp_fractional, value)
			}
			row[column_name] = res

		case FIELD_TYPE_VARCHAR:
			max_length := tableMap.columnMetaData[i].max_length
			var length int
			if max_length > 255 {
				var short uint16
				e = binary.Read(buf, binary.LittleEndian, &short)
				length = int(short)
			} else {
				var b byte
				b, e = buf.ReadByte()
				length = int(b)
			}
			if buf.Len() < length {
				e = io.EOF
			}
			row[column_name] = string(buf.Next(length))

		case FIELD_TYPE_STRING:
			var length int
			var b byte
			b, e = buf.ReadByte()
			length = int(b)
			row[column_name] = string(buf.Next(length))

		case FIELD_TYPE_ENUM:
			size := tableMap.columnMetaData[i].size
			var index int
			if size == 1 {
				var b byte
				b, e = buf.ReadByte()
				index = int(b)
			} else {
				index = int(bytesToUint16(buf.Next(int(size))))
			}
			row[column_name] = tableSchemaMap[i].enum_values[index-1]

		case FIELD_TYPE_SET:
			size := tableMap.columnMetaData[i].size
			var index int
			switch size {
			case 0:
				row[column_name] = nil
				break
			case 1:
				var b byte
				b, e = buf.ReadByte()
				index = int(b)
			case 2:
				index = int(bytesToUint16(buf.Next(int(size))))
			case 3:
				index = int(bytesToUint24(buf.Next(int(size))))
			case 4:
				index = int(bytesToUint32(buf.Next(int(size))))
			default:
				index = 0
			}
			result := make(map[string]int, 0)
			var mathPower = func (x int, n int) int {
					ans := 1
					for n != 0 {
						ans *= x
						n--
					}
					return ans
					}

			for i, val := range tableSchemaMap[i].set_values {
				s := index & mathPower(2,i)
				if s > 0 {
					result[val] = 1
				}
			}
			f := make([]string, 0)
			for key, _ := range result {
				f = append(f, key)
			}
			row[column_name] = f

		case FIELD_TYPE_BLOB,FIELD_TYPE_TINY_BLOB, FIELD_TYPE_MEDIUM_BLOB,
			FIELD_TYPE_LONG_BLOB, FIELD_TYPE_VAR_STRING:
			var length uint64
			length, e = readFixedLengthInteger(buf, int(tableMap.columnMetaData[i].length_size))
			row[column_name] = string(buf.Next(int(length)))
			break

		case FIELD_TYPE_BIT:
			var resp string = ""
			for k := 0; k < tableMap.columnMetaData[i].bytes; k++ {
				//var current_byte = ""
				var current_byte []string
				var b byte
				var end byte
				b, e = buf.ReadByte()
				var data int
				data = int(b)
				if k == 0 {
					if tableMap.columnMetaData[i].bytes == 1 {
						end = tableMap.columnMetaData[i].bits
					} else {
						end = tableMap.columnMetaData[i].bits % 8
						if end == 0 {
							end = 8
						}
					}
				} else {
					end = 8
				}
				var bit uint
				for bit = 0; bit < uint(end); bit++ {
					tmp := 1 << bit
					if (data & tmp) > 0 {
						current_byte = append(current_byte, "1")
					} else {
						current_byte = append(current_byte, "0")
					}
				}
				for k := len(current_byte); k > 0; k-- {
					resp += current_byte[k-1]
				}
			}
			bitInt, _ := strconv.ParseInt(resp, 2, 10)
			row[column_name] = bitInt
			break

		case
			FIELD_TYPE_GEOMETRY:
			return nil, fmt.Errorf("parseEventRow unimplemented for field type %s", fieldTypeName(tableMap.columnTypes[i]))

		case FIELD_TYPE_DATE, FIELD_TYPE_NEWDATE:
			var data []byte
			data = buf.Next(3)
			timeInt := int(int(data[0]) + (int(data[1]) << 8) + (int(data[2]) << 16))
			if timeInt == 0 {
				row[column_name] = nil
			} else {
				year := (timeInt & (((1 << 15) - 1) << 9)) >> 9
				month := (timeInt & (((1 << 4) - 1) << 5)) >> 5
				day := (timeInt & ((1 << 5) - 1))
				var monthStr, dayStr string
				if month >= 10 {
					monthStr = strconv.Itoa(month)
				} else {
					monthStr = "0" + strconv.Itoa(month)
				}
				if day >= 10 {
					dayStr = strconv.Itoa(day)
				} else {
					dayStr = "0" + strconv.Itoa(day)
				}
				t := strconv.Itoa(year) + "-" + monthStr + "-" + dayStr
				///tm, _ := time.Parse("2006-01-02", t)
				row[column_name] = t
			}

		case FIELD_TYPE_TIME:
			var data []byte
			data = buf.Next(3)
			timeInt := int(int(data[0]) + (int(data[1]) << 8) + (int(data[2]) << 16))
			if timeInt == 0 {
				row[column_name] = nil
			} else {
				hour := int(timeInt / 10000)
				minute := int((timeInt % 10000) / 100)
				second := int(timeInt % 100)
				var minuteStr, secondStr string
				if minute > 10 {
					minuteStr = strconv.Itoa(minute)
				} else {
					minuteStr = "0" + strconv.Itoa(minute)
				}
				if second > 10 {
					secondStr = strconv.Itoa(second)
				} else {
					secondStr = "0" + strconv.Itoa(second)
				}
				t := strconv.Itoa(hour) + ":" + minuteStr + ":" + secondStr
				//tm, _ := time.Parse("15:04:05", t)
				//row[column_name] = tm.Format("15:04:05")
				row[column_name] = t
			}

		case FIELD_TYPE_TIMESTAMP:
			var length int = 4
			timestamp := int64(bytesToUint32(buf.Next(length)))
			tm := time.Unix(timestamp, 0)
			//tm.Format(TIME_FORMAT)
			row[column_name] = tm.Format(TIME_FORMAT)

		case FIELD_TYPE_DATETIME:
			var t int64
			e = binary.Read(buf, binary.LittleEndian, &t)

			second := int(t % 100)
			minute := int((t % 10000) / 100)
			hour := int((t % 1000000) / 10000)

			d := int(t / 1000000)
			day := d % 100
			month := time.Month((d % 10000) / 100)
			year := d / 10000

			row[column_name] = time.Date(year, month, day, hour, minute, second, 0, time.UTC).Format(TIME_FORMAT)

		default:
			return nil, fmt.Errorf("Unknown FieldType %d", tableMap.columnTypes[i])
		}
		if e != nil {
			return nil, e
		}
	}
	return
}
