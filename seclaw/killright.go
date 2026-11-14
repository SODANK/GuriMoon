package seclaw

import "time"

// Kill rights, the classic (2006) rule: an unlawful kill in empire space gives the victim a right
// against the killer for thirty days, during which the victim may shoot the killer anywhere CONCORD
// polices without it being a crime. A fresh unlawful kill refreshes it. Killing the target's ship
// under the right spends it, leaving KillRightPodWindow to take the pod; a pod kill ends it at once.
const KillRightDuration = 30 * 24 * time.Hour

// KillRightPodWindow is how long a right spent on a ship kill still covers the pod.
const KillRightPodWindow = 2 * time.Minute

// ExemptKillRight names a shot made under a kill right.
const ExemptKillRight Exemption = "kill right held"

// KillRightExpiry is when a right granted at t lapses.
func KillRightExpiry(t time.Time) time.Time { return t.Add(KillRightDuration) }
