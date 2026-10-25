//go:build !unix

package main

// lockCredentials takes no lock where flock is not there: two cadres
// refreshing one login at once may then make each other log in again.
func lockCredentials() (unlock func(), err error) { return func() {}, nil }
