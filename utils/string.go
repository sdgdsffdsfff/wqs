/*
Copyright 2009-2016 Weibo, Inc.

All files licensed under the Apache License, Version 2.0 (the "License");
you may not use these files except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"unsafe"
)

// USE AT YOUR OWN RISK

// String force casts a []byte to a string.
// Used as hack for string() when str is used as temp val
func BytesToString(data []byte) string {
	if len(data) == 0 {
		return ""
	}

	return *(*string)(unsafe.Pointer(&data))
}

func StringToBytes(str string) []byte {
	strHeader := (*[2]uintptr)(unsafe.Pointer(&str))
	bytesHeader := [3]uintptr{strHeader[0], strHeader[1], strHeader[1]}
	return *(*[]byte)(unsafe.Pointer(&bytesHeader))
}
