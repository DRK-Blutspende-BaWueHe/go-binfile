package binfile

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

const (
	ANNOTATION_TRIM       = "trim"       // Trim the result (only for string)
	ANNOTATION_TERMINATOR = "terminator" // Annotate an indicator for ends fo lists (typically sth like CR or CRLF)
)

var ErrAbortArrayTerminator = fmt.Errorf("aborting due to array-terminator found")

// var ErrInvalidAddressAnnotation = fmt.Errorf("invalid address annotation")
var ErrAnnotatedFieldNotWritable = fmt.Errorf("annotated Field is not writable")

func Unmarshal(inputBytes []byte, target interface{}, enc Encoding, tz Timezone, arrayTerminator string) error {

	switch reflect.ValueOf(target).Kind() {
	case reflect.Ptr:
		nested := reflect.ValueOf(target).Elem()
		switch nested.Kind() {
		case reflect.Struct:
			targetStruct := reflect.ValueOf(target).Elem()
			_, err := internalUnmarshal(inputBytes, 0, targetStruct, arrayTerminator, 1, enc, tz)
			return err // its not a desaster if we accidentaly descended into a non-struct;
		case reflect.Slice:
			targetSlice := reflect.ValueOf(target).Elem()
			targetType := targetSlice.Type()

			currentByte := 0
			for {
				outputTarget := reflect.New(targetType.Elem())

				var err error
				lastByte := currentByte
				currentByte, err = internalUnmarshal(inputBytes, currentByte, outputTarget.Elem(), arrayTerminator, 1, enc, tz)

				if lastByte == currentByte {
					return nil // no further progress
				}

				if err == nil || err == ErrAbortArrayTerminator {
					// helpflul debugging
					// fmt.Printf("Adding slice %#v\n", outputTarget.Elem())
					targetSlice = reflect.Append(targetSlice, outputTarget.Elem())
					reflect.ValueOf(target).Elem().Set(targetSlice)
				}

				if currentByte >= len(inputBytes) {
					return nil // the end (do not move this lower in code, as the boundary check has to be first)
				}

				if err == ErrAbortArrayTerminator {
					continue // just the end of an array
				}

				if err != nil && err != ErrAbortArrayTerminator {
					return err // failed
				}

			}
		default:
			return fmt.Errorf("unable to unmarshal %s of type %s", reflect.TypeOf(target).Name(), reflect.TypeOf(target))
		}
	default:
		return fmt.Errorf("unable to unmarshal %s of type %s", reflect.TypeOf(target).Name(), reflect.TypeOf(target))
	}

}

// use this for recursion
func internalUnmarshal(inputBytes []byte, currentByte int, record reflect.Value, arrayTerminator string, depth int, enc Encoding, tz Timezone) (int, error) {

	var initialStartByte = currentByte

	for fieldNo := 0; fieldNo < record.NumField(); fieldNo++ {

		var recordField = record.Field(fieldNo)

		var binTag = record.Type().Field(fieldNo).Tag.Get("bin")
		if !recordField.CanInterface() {
			if binTag != "" {
				return currentByte, fmt.Errorf("field '%s' is not exported but annotated", record.Type().Field(fieldNo).Name)
			} else {
				continue // TODO: this won't notify you about accidentally not exported nested structs
			}
		}

		var annotationList, hasAnnotations = getAnnotationList(binTag)
		_ = hasAnnotations

		absoluteAnnotatedPos, relativeAnnotatedLength, hasAnnotatedAddress, err := getAddressAnnotation(annotationList)
		_ = hasAnnotatedAddress
		if err != nil {
			return currentByte, fmt.Errorf("invalid address annotation field '%s' `%s`: %w", record.Type().Field(fieldNo).Name, binTag, err)
		}

		hasTrimAnnotation := false
		if sliceContainsString(annotationList, ANNOTATION_TRIM) {
			hasTrimAnnotation = true
		}
		hasTerminatorAnnotation := false
		if sliceContainsString(annotationList, ANNOTATION_TERMINATOR) {
			hasTerminatorAnnotation = true
			if reflect.TypeOf(recordField.Interface()).Kind() != reflect.String {
				return currentByte, fmt.Errorf("array-terminator fields must be string ('%s')", record.Type().Field(fieldNo).Name)
			}
		}

		if absoluteAnnotatedPos > 0 {
			// The current field has an absolute Address. This causes the cursor to be forwarded
			currentByte = initialStartByte + absoluteAnnotatedPos
		}

		if relativeAnnotatedLength > 0 {
			// Having a length, the total length is not supposed to exceed the boundaries of the input
			if currentByte+relativeAnnotatedLength-1 > len(inputBytes) {
				return currentByte, fmt.Errorf("reading out of bounds position %d in input data of %d bytes", currentByte+relativeAnnotatedLength, len(inputBytes))
			}
		}

		// Really useful debugging:
		for k := 0; k < depth; k++ {
			fmt.Print(" ")
		}
		fmt.Printf("Field %s (%d:%d) with at %d \n",
			record.Type().Field(fieldNo).Name,
			absoluteAnnotatedPos, relativeAnnotatedLength, currentByte)

		if !recordField.CanInterface() {
			continue // field is not accessible
		}

		switch reflect.TypeOf(recordField.Interface()).Kind() {
		case reflect.Slice:
			switch reflect.TypeOf(recordField.Type()).Elem().Kind() { // Nested: all here is an array of something
			case reflect.Struct:

				targetType := recordField.Type()

				output := reflect.MakeSlice(targetType, 0, 0)
				recordField.Set(output)

				for {
					outputTarget := reflect.New(targetType.Elem())
					lastByte := currentByte
					var err error
					currentByte, err = internalUnmarshal(inputBytes, currentByte, outputTarget.Elem(), arrayTerminator, depth+1, enc, tz)

					if lastByte == currentByte { // we didnt progess a single byte
						break
					}

					output = reflect.Append(output, outputTarget.Elem())
					recordField.Set(output)

					if err == ErrAbortArrayTerminator {
						break // terminate because the annotated terminator-field was set
					}

					if currentByte >= len(inputBytes) { // read to an end = peaceful exit
						break // read further than the end
					}
				}

			default:
				return currentByte, fmt.Errorf("arrays of type '%s' are not supported (field '%s')", record.Type().Name(), record.Type().Field(fieldNo).Name)
			}

		case reflect.Struct:

			var err error
			currentByte, err = internalUnmarshal(inputBytes, currentByte, recordField, arrayTerminator, depth+1, enc, tz)
			if err != nil { // If the nested structure did fail, then bail out
				return currentByte, err
			}

		case reflect.String:

			if binTag == "" {
				continue // Do not process unannotated fields
			}

			if relativeAnnotatedLength < 0 { // Requires a valid length
				return currentByte, fmt.Errorf("invalid address annotation field '%s' `%s`", record.Type().Field(fieldNo).Name, binTag)
			}

			if !recordField.CanSet() {
				return currentByte, ErrAnnotatedFieldNotWritable
			}

			strvalue := string(inputBytes[currentByte : currentByte+relativeAnnotatedLength])

			if hasTerminatorAnnotation {
				if strvalue == arrayTerminator {
					reflect.ValueOf(recordField.Addr().Interface()).Elem().SetString(reflect.ValueOf(arrayTerminator).String())
					// Forward by annotated length only when terminator applies
					currentByte += relativeAnnotatedLength
					return currentByte, ErrAbortArrayTerminator
				}
				break
			}

			currentByte += relativeAnnotatedLength
			if hasTrimAnnotation {
				strvalue = strings.TrimSpace(strvalue)
			}

			reflect.ValueOf(recordField.Addr().Interface()).Elem().SetString(reflect.ValueOf(strvalue).String())

		case reflect.Int:

			if binTag == "" {
				continue // Do not process unannotated fields
			}

			if relativeAnnotatedLength < 0 { // Requires a valid length
				return currentByte, fmt.Errorf("invalid address annotation field '%s' `%s`", record.Type().Field(fieldNo).Name, binTag)
			}

			strvalue := string(inputBytes[currentByte : currentByte+relativeAnnotatedLength])
			currentByte += relativeAnnotatedLength

			strvalue = strings.TrimSpace(strvalue)

			num, err := strconv.Atoi(strvalue)
			if err != nil {
				return currentByte, err
			}

			reflect.ValueOf(recordField.Addr().Interface()).Elem().Set(reflect.ValueOf(num))

		case reflect.Float32:

			if binTag == "" {
				continue // Do not process unannotated fields
			}

			if relativeAnnotatedLength < 0 { // Requires a valid length
				return currentByte, fmt.Errorf("invalid address annotation field '%s' `%s`", record.Type().Field(fieldNo).Name, binTag)
			}

			value := string(inputBytes[currentByte : currentByte+relativeAnnotatedLength])
			currentByte += relativeAnnotatedLength
			strvalue := strings.TrimSpace(value)
			num, err := strconv.ParseFloat(strvalue, 32)
			if err != nil {
				return currentByte, err
			}

			reflect.ValueOf(recordField.Addr().Interface()).Elem().Set(reflect.ValueOf(float32(num)))

		case reflect.Float64:

			if binTag == "" {
				continue // Do not process unannotated fields
			}

			if relativeAnnotatedLength < 0 { // Requires a valid length
				return currentByte, fmt.Errorf("invalid address annotation field '%s' `%s`", record.Type().Field(fieldNo).Name, binTag)
			}

			value := string(inputBytes[currentByte : currentByte+relativeAnnotatedLength])
			currentByte += relativeAnnotatedLength
			strvalue := strings.TrimSpace(value)
			num, err := strconv.ParseFloat(strvalue, 64)
			if err != nil {
				return currentByte, err
			}

			reflect.ValueOf(recordField.Addr().Interface()).Elem().Set(reflect.ValueOf(float32(num)))

		default:

			if binTag != "" { // only if annotated this wil create an error
				return currentByte, fmt.Errorf("invalid type for field %s", record.Type().Field(fieldNo).Name)
			}
		}
	}

	return currentByte, nil
}

// find a searchstring within an array of strings. only matches full
// returns
//   - true if the string is present
//   - false otherwise
func sliceContainsString(list []string, search string) bool {
	for _, x := range list {
		if x == search {
			return true
		}
	}
	return false
}
