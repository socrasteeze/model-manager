package evict

// caseInsensitivePaths reflects how the platform's filesystems compare names.
//
// Windows and macOS default to case-insensitive; Linux does not. Getting this
// wrong in the permissive direction would let two different files compare equal
// on Linux, which for a call that deletes one of them is not an acceptable
// trade.
const caseInsensitivePaths = true
