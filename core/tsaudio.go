package main

// Whether a drive was recorded with the microphone is one of the first things you want
// from the index, and it has to mean the same thing whether the drive is sitting on this
// Mac or still on the comma. Locally we can just ask ffprobe. On the comma we can't —
// there's no ffmpeg on the device — and copying a whole qcamera.ts back just to look at
// it would make indexing crawl.
//
// But the answer is in the first few kilobytes. An MPEG transport stream declares its
// streams in a program map table that repeats every fraction of a second, so reading the
// head of the file is enough to see whether an audio stream was ever muxed in. That's a
// small enough read to fetch over SSH for every drive at once.

// Stream types that mean audio in a PMT. Anything else (video, private data, timing) is
// not what we're looking for.
var tsAudioStreamTypes = map[byte]bool{
	0x03: true, // MPEG-1 audio
	0x04: true, // MPEG-2 audio
	0x0f: true, // AAC (ADTS)
	0x11: true, // AAC (LATM)
	0x1c: true, // PCM
	0x81: true, // AC-3
	0x87: true, // E-AC-3
}

// tsHasAudio reports whether a transport stream declares an audio stream, and whether it
// could tell at all. It needs only the start of the file — enough to catch one PAT and
// the PMT it points at. The second return value matters: a read too short to contain a
// program table means "don't know", and reporting that as "no audio" would put a
// silent-drive marker on a drive that was recorded with the mic on.
func tsHasAudio(buf []byte) (audio bool, known bool) {
	const pkt = 188
	base := tsSyncOffset(buf)
	if base < 0 {
		return false, false
	}
	pmtPIDs := map[int]bool{}
	// Two passes: the PMT can appear before the PAT that names it, so collect the PMT
	// PIDs first and only then look for those tables.
	for pass := 0; pass < 2; pass++ {
		for off := base; off+pkt <= len(buf); off += pkt {
			p := buf[off : off+pkt]
			if p[0] != 0x47 {
				if base = tsSyncOffset(buf[off:]); base < 0 {
					break
				}
				base += off
				off = base - pkt // resync, then carry on from the new alignment
				continue
			}
			if p[1]&0x80 != 0 { // transport error — the packet's contents can't be trusted
				continue
			}
			pid := int(p[1]&0x1f)<<8 | int(p[2])
			if pass == 0 && pid != 0 {
				continue
			}
			if pass == 1 && !pmtPIDs[pid] {
				continue
			}
			sec := tsSection(p)
			if len(sec) < 4 {
				continue
			}
			if pass == 0 {
				collectPMTPIDs(sec, pmtPIDs)
				continue
			}
			hasAudio, ok := pmtStreams(sec)
			if !ok {
				continue
			}
			known = true
			if hasAudio {
				return true, true
			}
		}
		if pass == 0 && len(pmtPIDs) == 0 {
			return false, false // no program table in this stretch — can't say
		}
	}
	return false, known
}

// tsSyncOffset finds where the 188-byte packet grid starts. A file fetched from the
// device can begin mid-packet, and reading from the wrong offset yields nonsense.
func tsSyncOffset(buf []byte) int {
	const pkt = 188
	for i := 0; i < len(buf) && i < pkt*2; i++ {
		if buf[i] != 0x47 {
			continue
		}
		// One sync byte proves nothing (0x47 is a common payload byte); require the next
		// few packet boundaries to line up too.
		ok := true
		for n := 1; n <= 3; n++ {
			j := i + n*pkt
			if j >= len(buf) {
				break
			}
			if buf[j] != 0x47 {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// tsSection returns the start of a PSI section carried by this packet, or nil. Only the
// first packet of a section is used: tables that span packets are rarer than the
// repetition interval, so the next copy will do.
func tsSection(p []byte) []byte {
	if p[1]&0x40 == 0 { // not a payload-unit start, so no section header here
		return nil
	}
	afc := (p[3] >> 4) & 0x3
	if afc == 0 || afc == 2 { // no payload
		return nil
	}
	i := 4
	if afc == 3 {
		i += 1 + int(p[4]) // skip the adaptation field
	}
	if i >= len(p) {
		return nil
	}
	i += 1 + int(p[i]) // pointer_field
	if i >= len(p) {
		return nil
	}
	return p[i:]
}

// sectionBody validates a section header and returns the table body (excluding the CRC).
func sectionBody(sec []byte, tableID byte) []byte {
	if len(sec) < 8 || sec[0] != tableID {
		return nil
	}
	length := int(sec[1]&0x0f)<<8 | int(sec[2])
	end := 3 + length
	if end > len(sec) {
		end = len(sec) // truncated by our short read — use what arrived
	}
	if end-4 <= 8 {
		return nil
	}
	return sec[8 : end-4] // past the header, before the CRC
}

func collectPMTPIDs(sec []byte, out map[int]bool) {
	body := sectionBody(sec, 0x00) // PAT
	if body == nil {
		return
	}
	for i := 0; i+4 <= len(body); i += 4 {
		prog := int(body[i])<<8 | int(body[i+1])
		pid := int(body[i+2]&0x1f)<<8 | int(body[i+3])
		if prog != 0 { // program 0 points at the network table, not a program map
			out[pid] = true
		}
	}
}

// pmtStreams reports whether this program map lists an audio stream, and whether it was
// a readable program map at all.
func pmtStreams(sec []byte) (audio bool, ok bool) {
	body := sectionBody(sec, 0x02) // PMT
	if body == nil || len(body) < 4 {
		return false, false
	}
	infoLen := int(body[2]&0x0f)<<8 | int(body[3])
	i := 4 + infoLen
	for i+5 <= len(body) {
		if tsAudioStreamTypes[body[i]] {
			return true, true
		}
		esLen := int(body[i+3]&0x0f)<<8 | int(body[i+4])
		i += 5 + esLen
	}
	return false, true
}
