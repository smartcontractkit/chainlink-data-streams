// Package dev documents the llo/dev tree. It contains no code.
//
// Everything under llo/dev is experimental and pre-release. These packages
// target protocol versions that have not shipped, and they exist here so that
// their lifecycle status is visible in the import path.
//
// No API stability guarantee applies: exported symbols under llo/dev may
// change, move or disappear in any release, without a major version bump and
// without a deprecation period. Do not depend on them from production code.
//
// A package graduates by moving up to llo/ (for example llo/dev/v31 becomes
// llo/v31) once its protocol version ships; that move is the point at which
// the normal compatibility promise starts to apply.
package dev
