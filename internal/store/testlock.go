package store

// TestLock is the Postgres advisory lock the database integration tests hold,
// so tests in different packages don't empty and fill the test database at
// the same time.
const TestLock = 7_062_026
