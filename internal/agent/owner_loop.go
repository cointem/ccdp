package agent

import (
	"ccdp/internal/protocol"
)

// Run is the agent's main command loop. It is the sole consumer of commands.
// Run is idempotent: callers may start it explicitly, while the first Submit
// also starts it when needed.
func (a *Agent) Run() {
	a.owner.once.Do(func() {
		a.mu.Lock()
		a.lifecycle.runStartedFlag = true
		a.refreshSnapshotLocked()
		a.mu.Unlock()
		close(a.owner.started)
		if a.services.supervisorOwner && a.services.supervisor != nil {
			a.services.supervisor.startRecovery()
		}
		defer close(a.owner.done)
		controlBurst := 0
		for {
			a.installPersistenceFailureOwned()
			a.drainRejectedCommands()
			a.drainChildRecords()
			a.drainOperationCompletions()
			a.drainViews()
			for _, barrier := range a.owner.barriers {
				close(barrier.ready)
			}
			a.owner.barriers = nil
			commands := a.owner.commands
			if len(a.owner.deferred) >= cap(a.owner.commands) {
				commands = nil
			}
			select {
			case <-a.owner.stop:
				return
			default:
			}
			// Control messages bypass even a full deferred ordinary-command queue.
			if controlBurst < 8 {
				select {
				case message := <-a.owner.control:
					a.handleOwnerControl(message, false)
					controlBurst++
					continue
				default:
				}
			}
			if len(a.owner.deferred) > 0 && !a.settingsPreparationPending() {
				req := a.owner.deferred[0]
				a.owner.deferred[0] = runtimeCommand{}
				a.owner.deferred = a.owner.deferred[1:]
				a.dispatchOwned(req)
				controlBurst = 0
				continue
			}
			select {
			case <-a.owner.stop:
				return
			case message := <-a.owner.control:
				a.handleOwnerControl(message, false)
				controlBurst++
			case req := <-commands:
				a.drainOperationCompletions()
				a.dispatchOwned(req)
				controlBurst = 0
			}
		}
	})
}

func (a *Agent) dispatchOwned(req runtimeCommand) {
	if settingsMutationCommand(req.command.Type) && a.settingsPreparationPending() {
		a.owner.deferred = append(a.owner.deferred, req)
		return
	}
	var receipt protocol.Receipt
	if a.lifecycle.rootCtx.Err() != nil {
		receipt = a.rejectedReceipt(req.command, protocol.ErrorClosed, "session is closed")
	} else {
		receipt = a.dispatchCommandForTurn(req.command, true, req.cancellationTurn)
	}
	a.refreshSnapshotOwned()
	a.drainViews()
	select {
	case req.result <- receipt:
	default:
	}
}

func (a *Agent) handleOwnerControl(message ownerControl, preparing bool) {
	defer a.refreshSnapshotOwned()
	switch message := message.(type) {
	case controlCommand:
		a.drainRejectedCommands()
		a.dispatchOwned(message.request)
	case quiescenceRequested:
		scope, err := a.beginSandboxQuiescence(message.ctx, message.closeMCP)
		replyOwned(a, message.result, quiescenceResult{scope, err})
	case quiescenceAborted:
		a.abortQuiescenceOwned(message.revision)
		close(message.done)
	case auxiliaryFactPrepared:
		replyOwned(a, message.result, a.commitAuxiliaryOwned(message))
	case memoryClearRequested:
		replyOwned(a, message.result, a.clearMemoryOwned())
	case directoryAccessRequested:
		a.setDirectoryAccessOwned(message)
		close(message.done)
	case turnStartRequested:
		replyOwned(a, message.result, a.persistTurnStarted(message.turnID, message.stepID, message.inputID))
	case closeStarted:
		a.beginCloseOwned()
		a.refreshSnapshotOwned()
		close(message.done)
	case writerCloseRequested:
		replyOwned(a, message.result, a.closePersistence())
	case closeFinished:
		a.finishCloseOwned(message.err)
		close(message.done)
	case compactionFailed:
		a.rememberCompactFailureOwned(message)
		close(message.done)
	case runInputsCancelled:
		replyOwned(a, message.result, a.cancelRunInputs(message.runID))
	case sessionCapabilitiesRequested:
		policy, version, err := a.addSessionCapabilitiesOwned(message.version, message.capabilities)
		replyOwned(a, message.result, sessionCapabilitiesResult{policy: policy, version: version, err: err})
	case guardianSlotRequested:
		replyOwned(a, message.result, a.takeGuardianSlotOwned())
	case toolOutputRequested:
		replyOwned(a, message.result, a.clipAggregateOwned(message.text))
	case toolsDiscoveredPrepared:
		replyOwned(a, message.result, a.installDiscoveredTools(message))
	case tasksPrepared:
		replyOwned(a, message.result, a.commitTasks(message))
	case historyReplacementPrepared:
		replyOwned(a, message.result, a.installHistoryReplacement(message))
	case hookStartedRequested:
		replyOwned(a, message.result, a.commitHookStart(message.invocation))
	case hookFinishedPrepared:
		replyOwned(a, message.result, a.commitHookResult(message))
	case turnOutcomePrepared:
		replyOwned(a, message.result, a.settleTurnOutcome(message))
	case turnCleanupRequested:
		replyOwned(a, message.result, a.prepareTurnCleanup())
	case turnPhaseChanged:
		a.setPhaseOwned(message.phase)
		a.refreshSnapshotOwned()
		close(message.done)
	case fallbackRequested:
		replyOwned(a, message.result, a.selectFallback())
	case turnNoteRecorded:
		a.recordTurnNote(message)
		a.refreshSnapshotOwned()
		close(message.done)
	case goalTransitionRequested:
		replyOwned(a, message.result, a.applyGoalTransition(message))
	case goalAttemptCompleted:
		replyOwned(a, message.result, a.accountGoalFinishedOwned(message.manifest, message.attempt, true))
	case sharedAttemptCompleted:
		replyOwned(a, message.result, a.accountSharedAttempt(message.attempt))
	case mailboxAdmission:
		replyOwned(a, message.result, a.applySubmitInput(message.command))
	case stepRequested:
		step, err := a.captureStepOwned()
		replyOwned(a, message.result, stepResult{step: step, err: err})
	case stepReleased:
		a.clearStepContext(message.turn, message.step)
		a.refreshSnapshotOwned()
		close(message.done)
	case toolStartRequested:
		replyOwned(a, message.result, a.acceptToolStart(message))
	case toolCompleted:
		replyOwned(a, message.result, a.commitToolResult(message))
	case toolProjectionRequested:
		replyOwned(a, message.result, a.projectToolsOwned(message))
	case requestPrepared:
		replyOwned(a, message.result, a.commitPreparedRequest(message))
	case historyAppendRequested:
		replyOwned(a, message.result, a.commitHistoryAppend(message))
	case inputClaimRequested:
		input, delivered, err := a.claimPendingInput(message.turnID, message.strategy, true)
		replyOwned(a, message.result, inputClaimResult{input: input, delivered: delivered, err: err})
	case *decisionRequest:
		a.enqueueDecision(message)
	case decisionCancelled:
		a.cancelDecision(message.request)
	case requestCompleted:
		settlement, err := a.settleRequestCompleted(message)
		replyOwned(a, message.result, requestSettlementResult{settlement: settlement, err: err})
	case viewsReady:
		a.drainViews()
	case streamsReady:
		a.mu.Lock()
		if a.publications.streamTimerGeneration == message.generation {
			a.flushStreamsLocked()
		}
		a.mu.Unlock()
		a.drainViews()
	case watchRequested:
		a.registerWatch(message)
	case watchRemoved:
		a.viewHubOwned().Remove(message.id)
		a.refreshSnapshotOwned()
		close(message.done)
	case viewsClosed:
		a.mu.Lock()
		a.flushStreamsLocked()
		a.mu.Unlock()
		a.drainViews()
		a.viewHubOwned().Close()
		a.refreshSnapshotOwned()
		close(message.done)
	case settingsReservationRequested:
		workspace, err := a.reserveSettingsMutationOwned()
		replyOwned(a, message.result, settingsReservationResult{workspace: workspace, err: err})
	case settingsReservationReleased:
		a.releaseSettingsMutationOwned()
		a.refreshSnapshotOwned()
		close(message.done)
	case executionModeRequested:
		replyOwned(a, message.result, a.installExecutionMode(message.on))
	case settingsCandidateInstall:
		replyOwned(a, message.result, a.installSettingsCandidate(message))
	case sessionClosingPrepared:
		replyOwned(a, message.result, a.commitSessionClosing(message.outcome))
	case childOutboxPrepared:
		replyOwned(a, message.result, a.commitChildOutbox(message))
	case completionsReady:
		a.drainChildRecords()
		a.drainOperationCompletions()
	case turnCompleted:
		a.completeTurn(message)
	case finalizeBarrier:
		a.drainChildRecords()
		a.drainOperationCompletions()
		if preparing {
			a.owner.barriers = append(a.owner.barriers, message)
		} else {
			close(message.ready)
		}
	}
}
