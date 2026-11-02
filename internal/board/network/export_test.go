package network

// SetBeforePush makes the next push run hook between its fetch and its push.
func (s *Syncer) SetBeforePush(hook func()) { s.beforePush = hook }
