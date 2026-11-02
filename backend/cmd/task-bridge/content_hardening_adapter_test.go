package main
import("context"; "bba.local/taskengine/internal/taskengine")
type hardHTTPJoinAdapter struct{actor taskengine.RuntimeActor;gateway *gatewayServer}
func(a hardHTTPJoinAdapter)RequestJoin(ctx context.Context,r taskengine.Reservation)(taskengine.SendOutcome,error){return a.gateway.RequestJoin(ctx,a.actor,r)}
