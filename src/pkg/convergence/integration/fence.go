package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// writerLock fences multiple hub processes and catches up only the appended
// suffix before making a decision. No forge I/O runs while this lock is held.
func (s *Store) writerLock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	unlock := func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }
	f, err := os.Open(s.path)
	if os.IsNotExist(err) && s.offset == 0 {
		return unlock, nil
	}
	if err != nil {
		unlock()
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		unlock()
		return nil, err
	}
	if stat.Size() < s.offset {
		unlock()
		return nil, fmt.Errorf("integration journal truncated; refusing stale writer")
	}
	if stat.Size() > maxJournalBytes {
		unlock()
		return nil, fmt.Errorf("integration journal exceeds bounded replay size")
	}
	if stat.Size() == s.offset {
		return unlock, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], stat.Size()-1); err != nil || last[0] != '\n' {
		unlock()
		return nil, fmt.Errorf("integration journal has an incomplete concurrent record")
	}
	if _, err := f.Seek(s.offset, 0); err != nil {
		unlock()
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), maxJournalRecordBytes)
	for scanner.Scan() {
		if len(s.seen) > maxJournalRecords {
			unlock()
			return nil, fmt.Errorf("integration journal exceeds bounded replay index")
		}
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			unlock()
			return nil, err
		}
		if err := s.validate(event); err != nil {
			unlock()
			return nil, err
		}
		s.apply(event)
		s.offset += int64(len(scanner.Bytes()) + 1)
	}
	if err := scanner.Err(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}
