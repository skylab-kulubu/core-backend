package ticket

// AttendanceMailOn answers which of the door's mails a scan queues now
// (NotifyAttendance, docs/attendance-mail.md): off until the worker starts,
// and for good when ATTENDANCE_MAIL is off, SkyMail is not configured or the
// kind's template key is empty. The admin panel's switches read it through
// GET /v1/users/me/capabilities (features).
func (s *PostgresStore) AttendanceMailOn() (welcome, recorded bool) {
	return s.mailWelcome.Load(), s.mailRecorded.Load()
}
