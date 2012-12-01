// Grumble - an implementation of Murmur in Go
// Copyright (c) 2010 The Grumble Authors
// The use of this source code is goverened by a BSD-style
// license that can be found in the LICENSE-file.

package cryptstate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"mumbleapp.com/grumble/pkg/cryptstate/ocb2"
	"time"
)

const DecryptHistorySize = 0x100

type CryptState struct {
	RawKey         [aes.BlockSize]byte
	EncryptIV      [ocb2.NonceSize]byte
	DecryptIV      [ocb2.NonceSize]byte
	decryptHistory [DecryptHistorySize]byte

	LastGoodTime int64

	Good         uint32
	Late         uint32
	Lost         uint32
	Resync       uint32
	RemoteGood   uint32
	RemoteLate   uint32
	RemoteLost   uint32
	RemoteResync uint32

	cipher cipher.Block
}

func New() (cs *CryptState, err error) {
	cs = new(CryptState)

	return
}

func (cs *CryptState) GenerateKey() (err error) {
	rand.Read(cs.RawKey[0:])
	rand.Read(cs.EncryptIV[0:])
	rand.Read(cs.DecryptIV[0:])

	cs.cipher, err = aes.NewCipher(cs.RawKey[0:])
	if err != nil {
		return
	}

	return
}

func (cs *CryptState) SetKey(key []byte, eiv []byte, div []byte) (err error) {
	if copy(cs.RawKey[0:], key[0:]) != aes.BlockSize {
		err = errors.New("Unable to copy key")
		return
	}

	if copy(cs.EncryptIV[0:], eiv[0:]) != ocb2.NonceSize {
		err = errors.New("Unable to copy EIV")
		return
	}

	if copy(cs.DecryptIV[0:], div[0:]) != ocb2.NonceSize {
		err = errors.New("Unable to copy DIV")
		return
	}

	cs.cipher, err = aes.NewCipher(cs.RawKey[0:])
	if err != nil {
		return
	}

	return
}

func (cs *CryptState) Decrypt(dst, src []byte) (err error) {
	if len(src) < 4 {
		err = errors.New("Crypted length too short to decrypt")
		return
	}

	plain_len := len(src) - 4
	if len(dst) != plain_len {
		err = errors.New("plain_len and src len mismatch")
		return
	}

	var saveiv [ocb2.NonceSize]byte
	var tag [ocb2.TagSize]byte
	var ivbyte byte
	var restore bool
	lost := 0
	late := 0

	ivbyte = src[0]
	restore = false

	if copy(saveiv[0:], cs.DecryptIV[0:]) != ocb2.NonceSize {
		err = errors.New("Copy failed")
		return
	}

	if byte(cs.DecryptIV[0]+1) == ivbyte {
		// In order as expected
		if ivbyte > cs.DecryptIV[0] {
			cs.DecryptIV[0] = ivbyte
		} else if ivbyte < cs.DecryptIV[0] {
			cs.DecryptIV[0] = ivbyte
			for i := 1; i < ocb2.NonceSize; i++ {
				cs.DecryptIV[i] += 1
				if cs.DecryptIV[i] > 0 {
					break
				}
			}
		} else {
			err = errors.New("invalid ivbyte")
		}
	} else {
		// Out of order or repeat
		var diff int
		diff = int(ivbyte - cs.DecryptIV[0])
		if diff > 128 {
			diff = diff - 256
		} else if diff < -128 {
			diff = diff + 256
		}

		if ivbyte < cs.DecryptIV[0] && diff > -30 && diff < 0 {
			// Late packet, but no wraparound
			late = 1
			lost = -1
			cs.DecryptIV[0] = ivbyte
			restore = true
		} else if ivbyte > cs.DecryptIV[0] && diff > -30 && diff < 0 {
			// Last was 0x02, here comes 0xff from last round
			late = 1
			lost = -1
			cs.DecryptIV[0] = ivbyte
			for i := 1; i < ocb2.NonceSize; i++ {
				cs.DecryptIV[i] -= 1
				if cs.DecryptIV[i] > 0 {
					break
				}
			}
			restore = true
		} else if ivbyte > cs.DecryptIV[0] && diff > 0 {
			// Lost a few packets, but beyond that we're good.
			lost = int(ivbyte - cs.DecryptIV[0] - 1)
			cs.DecryptIV[0] = ivbyte
		} else if ivbyte < cs.DecryptIV[0] && diff > 0 {
			// Lost a few packets, and wrapped around
			lost = int(256 - int(cs.DecryptIV[0]) + int(ivbyte) - 1)
			cs.DecryptIV[0] = ivbyte
			for i := 1; i < ocb2.NonceSize; i++ {
				cs.DecryptIV[i] += 1
				if cs.DecryptIV[i] > 0 {
					break
				}
			}
		} else {
			err = errors.New("No matching ivbyte")
			return
		}

		if cs.decryptHistory[cs.DecryptIV[0]] == cs.DecryptIV[0] {
			if copy(cs.DecryptIV[0:], saveiv[0:]) != ocb2.NonceSize {
				err = errors.New("Failed to copy ocb2.NonceSize bytes")
				return
			}
		}
	}

	ocb2.Decrypt(cs.cipher, dst[0:], src[4:], cs.DecryptIV[0:], tag[0:])

	for i := 0; i < 3; i++ {
		if tag[i] != src[i+1] {
			if copy(cs.DecryptIV[0:], saveiv[0:]) != ocb2.NonceSize {
				err = errors.New("Error while trying to recover from error")
				return
			}
			err = errors.New("tag mismatch")
			return
		}
	}

	cs.decryptHistory[cs.DecryptIV[0]] = cs.DecryptIV[0]

	if restore {
		if copy(cs.DecryptIV[0:], saveiv[0:]) != ocb2.NonceSize {
			err = errors.New("Error while trying to recover IV")
			return
		}
	}

	cs.Good += 1
	if late > 0 {
		cs.Late += uint32(late)
	} else {
		cs.Late -= uint32(-late)
	}
	if lost > 0 {
		cs.Lost = uint32(lost)
	} else {
		cs.Lost = uint32(-lost)
	}

	cs.LastGoodTime = time.Now().Unix()

	return
}

func (cs *CryptState) Encrypt(dst, src []byte) {
	var tag [ocb2.TagSize]byte

	// First, increase our IV
	for i := range cs.EncryptIV {
		cs.EncryptIV[i] += 1
		if cs.EncryptIV[i] > 0 {
			break
		}
	}

	ocb2.Encrypt(cs.cipher, dst[4:], src, cs.EncryptIV[0:], tag[0:])

	dst[0] = cs.EncryptIV[0]
	dst[1] = tag[0]
	dst[2] = tag[1]
	dst[3] = tag[2]

	return
}