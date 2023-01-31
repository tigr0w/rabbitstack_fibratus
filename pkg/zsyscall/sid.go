/*
 * Copyright 2021-2022 by Nedim Sabic Sabic
 * https://www.fibratus.io
 * All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package zsyscall

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

func LookupAccount(rawSid []byte, wbemSID bool) (string, string) {
	b := uintptr(unsafe.Pointer(&rawSid[0]))
	if wbemSID {
		// a WBEM SID is actually a TOKEN_USER structure followed
		// by the SID, so we have to double the pointer size
		b += uintptr(8 * 2)
	}
	sid := (*windows.SID)(unsafe.Pointer(b))
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return "", ""
	}
	return account, domain
}
