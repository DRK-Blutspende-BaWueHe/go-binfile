package binfile

import (
	"reflect"
	"strconv"
)

// Accepts an annotated struct or slice of structs.
//
// Returns a byte array with the converted contents or an error.
//
// Check the README.md for usage.
func Marshal(target interface{}, padding byte, enc Encoding, tz Timezone, arrayTerminator string) ([]byte, error) {

	// TODO: accepting a Ptr here is confusing as the func will not change the contents
	if reflect.TypeOf(target).Kind() == reflect.Ptr {
		return Marshal(reflect.ValueOf(target).Elem(), padding, enc, tz, arrayTerminator)
	}

	var outBytes []byte
	var err error
	var depth = 0

	var targetValue = reflect.ValueOf(target)
	var targetKind = targetValue.Kind()

	switch targetKind {
	case reflect.Slice:
		var innerValueKind = reflect.TypeOf(targetValue.Interface()).Elem().Kind()

		for i := 0; i < targetValue.Len(); i++ {
			var tempBytes []byte

			switch innerValueKind {
			case reflect.Slice:
				// TODO: slice of slices?

			case reflect.Struct:
				tempBytes, _, err = internalMarshal(targetValue.Index(i), false, padding, arrayTerminator, 0, depth+1)
				if err != nil {
					return []byte{}, err
				}
				outBytes = append(outBytes, tempBytes...)

			default:
				return []byte{}, newUnsupportedTypeError(reflect.TypeOf(targetValue.Interface()))
			}

			// for separating messages
			outBytes = append(outBytes, []byte(arrayTerminator)...)
		}

		return outBytes, err

	case reflect.Struct:
		outBytes, _, err = internalMarshal(targetValue, false, padding, arrayTerminator, 0, depth)
		return outBytes, err

	}

	return []byte{}, newUnsupportedTypeError(targetValue.Type())
}

// use this for recursion
func internalMarshal(record reflect.Value, onlyPaddWithZeros bool, padding byte, arrayTerminator string, currentByte int, depth int) ([]byte, int, error) {

	outBytes := []byte{}

	for fieldNo := 0; fieldNo < record.NumField(); fieldNo++ {

		var recordField = record.Field(fieldNo)

		var binTag = record.Type().Field(fieldNo).Tag.Get("bin")
		if !recordField.CanInterface() {
			if binTag != "" {
				return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, ErrorExportedFieldNotAnnotated)
			} else {
				continue // TODO: this won't notify you about accidentally not exported nested structs
			}
		}

		var annotationList, hasAnnotations = getAnnotationList(binTag)

		absoluteAnnotatedPos, relativeAnnotatedLength, hasAnnotatedAddress, err := getAddressAnnotation(annotationList)
		if err != nil {
			return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, newInvalidAddressAnnotationError(err))
		}

		if absoluteAnnotatedPos != -1 {
			if currentByte < absoluteAnnotatedPos {
				outBytes, currentByte = appendPaddingBytes(outBytes, absoluteAnnotatedPos-currentByte, padding)
			} else if currentByte > absoluteAnnotatedPos {
				return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, newInvalidInvalidOffsetError(currentByte, absoluteAnnotatedPos))
			}
		}
		/*
			for k := 0; k < depth; k++ {
				fmt.Print(" ")
			}
			fmt.Printf("Field %s (%d:%d) with at %d \n",
				record.Type().Field(fieldNo).Name,
				absoluteAnnotatedPos, relativeAnnotatedLength, currentByte)*/

		var valueKind = reflect.TypeOf(recordField.Interface()).Kind()
		if valueKind == reflect.Struct {

			var tempOutByte []byte
			var err error
			tempOutByte, currentByte, err = internalMarshal(recordField, onlyPaddWithZeros, padding, arrayTerminator, currentByte, depth+1)
			if err != nil { // If the nested structure did fail, then bail out
				return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, err)
			}

			outBytes = append(outBytes, tempOutByte...)

			continue
		}

		if !hasAnnotations {
			continue // Do not process unannotated fields
		}

		if valueKind == reflect.Slice {

			var arrayAnnotation, hasArrayAnnotation = getArrayAnnotation(annotationList)
			if !hasArrayAnnotation {
				return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, ErrorMissingArrayAnnotation)
			}

			var sliceValue = reflect.ValueOf(recordField.Interface())
			var innerValueKind = reflect.TypeOf(recordField.Interface()).Elem().Kind()

			if innerValueKind != reflect.Struct && !hasAnnotatedAddress {
				return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, ErrorMissingAddressAnnotation)
			}

			var arraySize = sliceValue.Len()
			var isTerminatorType = isArrayTypeTerminator(arrayAnnotation)
			if !isTerminatorType {
				if size, isFixedSize := getArrayFixedSize(arrayAnnotation); isFixedSize {
					arraySize = size
				} else if fieldName, isDynamic := getArraySizeFieldName(arrayAnnotation); isDynamic {
					arraySize, err = resolveDynamicArraySize(record, fieldName)
					if err != nil {
						return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, newInvalidDynamicArraySizeError(record.Type().Name(), fieldName, err))
					}
				}
			}

			var tempOutByte []byte
			var err error
			for i := 0; i < arraySize; i++ {

				var currentElement reflect.Value
				if i < sliceValue.Len() {
					currentElement = sliceValue.Index(i)
				} else {
					currentElement = reflect.New(reflect.TypeOf(recordField.Interface()).Elem()).Elem()
					onlyPaddWithZeros = true
				}

				switch innerValueKind {
				case reflect.Struct:

					tempOutByte, currentByte, err = internalMarshal(currentElement, onlyPaddWithZeros, padding, arrayTerminator, currentByte, depth+1)
					if err != nil {
						return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, err)
					}
					outBytes = append(outBytes, tempOutByte...)

				default:

					tempOutByte, currentByte, err = marshalSimpleTypes(currentElement, onlyPaddWithZeros, relativeAnnotatedLength, annotationList, currentByte, depth)
					if err != nil {
						return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, err)
					}
					outBytes = append(outBytes, tempOutByte...)
				}

			}
			onlyPaddWithZeros = false

			// TODO: why do we need the terminator in the 2nd case here?
			if isTerminatorType || sliceValue.Len() > arraySize {
				outBytes = append(outBytes, arrayTerminator...)
				currentByte += len(arrayTerminator)
			}

			continue
		}

		if !hasAnnotatedAddress {
			return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, ErrorMissingAddressAnnotation)
		}

		var tempOutByte []byte
		tempOutByte, currentByte, err = marshalSimpleTypes(recordField, onlyPaddWithZeros, relativeAnnotatedLength, annotationList, currentByte, depth)
		if err != nil {
			return []byte{}, currentByte, newProcessingFieldError(record.Type().Field(fieldNo).Name, binTag, err)
		}
		outBytes = append(outBytes, tempOutByte...)

	}

	return outBytes, currentByte, nil
}

// use this for processing end nodes
func marshalSimpleTypes(recordField reflect.Value, onlyPaddWithZeros bool, relativeAnnotatedLength int, annotationList []string, currentByte int, depth int) ([]byte, int, error) {

	if onlyPaddWithZeros {
		return make([]byte, relativeAnnotatedLength), currentByte + relativeAnnotatedLength, nil
	}

	var outBytes = []byte{}

	var valueKind = reflect.TypeOf(recordField.Interface()).Kind()
	switch valueKind {
	case reflect.String:

		var tempBytes = []byte(recordField.String())
		if len(tempBytes) > relativeAnnotatedLength {
			return []byte{}, currentByte, newInvalidValueLengthError(string(tempBytes), len(tempBytes))
		} else if len(tempBytes) < relativeAnnotatedLength {
			outBytes, _ = appendPaddingBytes(outBytes, relativeAnnotatedLength-len(tempBytes), byte(' '))
		}

		outBytes = append(outBytes, tempBytes...)
		currentByte += relativeAnnotatedLength

	case reflect.Int:

		// checks overflow - if system uses int32 as default
		var tempInt = int(recordField.Int())
		if int64(tempInt) != recordField.Int() {
			return []byte{}, currentByte, ErrorIntConversionOverflow
		}

		var isSignForced = hasAnnotationForceSign(annotationList)
		var isNegative = tempInt < 0
		if isNegative {
			outBytes = append(outBytes, '-')
		} else if isSignForced {
			outBytes = append(outBytes, '+')
		}

		var tempBytes = []byte(strconv.Itoa(tempInt))
		if isNegative { // handle negative sign separately
			tempBytes = tempBytes[1:]
		}

		var currLength = len(tempBytes)
		if isNegative || isSignForced {
			currLength++
		}

		if currLength > relativeAnnotatedLength {
			return []byte{}, currentByte, newInvalidValueLengthError(string(append(outBytes, tempBytes...)), currLength)
		} else if currLength < relativeAnnotatedLength {
			var paddingByte byte
			if hasAnnotationPadspace(annotationList) {
				paddingByte = byte(' ')
			} else {
				paddingByte = byte('0')
			}
			outBytes, _ = appendPaddingBytes(outBytes, relativeAnnotatedLength-currLength, paddingByte)
		}

		outBytes = append(outBytes, tempBytes...)
		currentByte += relativeAnnotatedLength

	case reflect.Float32, reflect.Float64:

		var precision = -1
		var err error
		if precision, err = getPrecisionFromAnnotation(annotationList); err != nil {
			return []byte{}, currentByte, err
		}

		var tempFloat = recordField.Float()
		var tempStr string
		if valueKind == reflect.Float32 {
			tempStr = strconv.FormatFloat(tempFloat, 'f', precision, 32)
		} else {
			tempStr = strconv.FormatFloat(tempFloat, 'E', precision, 64)
		}
		if tempFloat == float64(int(tempFloat)) { // is truly an int?
			if relativeAnnotatedLength > 1 {
				tempStr += "."
			}
		}

		var isSignForced = hasAnnotationForceSign(annotationList)
		var isNegative = tempFloat < 0
		if isNegative {
			outBytes = append(outBytes, '-')
		} else if isSignForced {
			outBytes = append(outBytes, '+')
		}

		var tempBytes = []byte(tempStr)
		if isNegative { // handle negative sign separately
			tempBytes = tempBytes[1:]
		}

		var currLength = len(tempBytes)
		if isNegative || isSignForced {
			currLength++
		}

		if currLength > relativeAnnotatedLength {
			return []byte{}, currentByte, newInvalidValueLengthError(string(append(outBytes, tempBytes...)), currLength)
		} else if currLength < relativeAnnotatedLength {
			var paddingByte byte
			if hasAnnotationPadspace(annotationList) {
				paddingByte = byte(' ')
			} else {
				paddingByte = byte('0')
			}
			outBytes, _ = appendPaddingBytes(outBytes, relativeAnnotatedLength-currLength, paddingByte)
		}

		outBytes = append(outBytes, tempBytes...)
		currentByte += relativeAnnotatedLength

	default:

		return []byte{}, currentByte, newUnsupportedTypeError(reflect.TypeOf(recordField.Interface()))
	}

	return outBytes, currentByte, nil
}
