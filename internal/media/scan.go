package media

// scannerAvailable is whether core has a malware scanner. It has none until
// media redesign ticket 12 (ClamAV): until then a purpose whose catalogue
// entry has scan: true (answer_file today) is refused with
// ErrPurposeNeedsScanner, because a Media that needs a scan is not opened
// before it is clean.
const scannerAvailable = false
