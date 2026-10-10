package update

// RestartInstructions returns how to restart the agent on each
// platform's service manager after a binary swap. The update command
// prints these — it never restarts the service itself.
func RestartInstructions(goos string) string {
	switch goos {
	case "darwin":
		return "  service agent:  localmesh-agent service stop && localmesh-agent service start\n" +
			"  (launchd label net.localmesh.agent; kickstart -k works too)\n" +
			"  foreground:     restart your `localmesh-agent run` process"
	case "linux":
		return "  service agent:  localmesh-agent service stop && localmesh-agent service start\n" +
			"  (or: systemctl --user restart localmesh-agent.service)\n" +
			"  foreground:     restart your `localmesh-agent run` process"
	case "windows":
		return "  service agent:  localmesh-agent service stop && localmesh-agent service start\n" +
			"  (or: sc.exe stop LocalMeshAgent && sc.exe start LocalMeshAgent)\n" +
			"  note: replace the binary only while the service is stopped"
	default:
		return "  restart the agent however it was launched"
	}
}
