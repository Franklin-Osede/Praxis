package ui

import (
	"praxis/internal/challenge"
	"praxis/internal/market"
	"praxis/internal/session"
)

func internalPilotConfig() session.Config {
	return session.Config{
		Instrument:               market.Instrument{Symbol: "MNQ", CentsPerTick: 50},
		SubjectID:                "t-01",
		RunID:                    "r-01",
		Pacing:                   session.PacingPilot,
		StartingBalanceCts:       5_000_000,
		CommissionPerContractCts: 50,
		Rules: challenge.Rules{
			StartingBalanceCts: 5_000_000, MaxDailyLossCts: 100_000,
			ProfitTargetCts: 100_000, MaxTotalLossCts: 200_000,
		},
	}
}
