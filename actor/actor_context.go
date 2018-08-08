package actor

import (
	"errors"
	"time"

	"github.com/AsynkronIT/protoactor-go/log"
	"github.com/emirpasic/gods/stacks/linkedliststack"
)

type contextState int32

const (
	stateNone contextState = iota
	stateAlive
	stateRestarting
	stateStopping
	stateStopped
)

type actorContextExtras struct {
	children            PIDSet
	receiveTimeoutTimer *time.Timer
	rs                  *RestartStatistics
	stash               *linkedliststack.Stack
	watchers            PIDSet
	context             Context
}

func newActorContextExtras(context Context) *actorContextExtras {
	this := &actorContextExtras{
		context: context,
	}
	return this
}

func (this *actorContextExtras) restartStats() *RestartStatistics {
	//lazy initialize the child restart stats if this is the first time
	//further mutations are handled within "restart"
	if this.rs == nil {
		this.rs = NewRestartStatistics()
	}
	return this.rs
}

func (this *actorContextExtras) initReceiveTimeoutTimer(timer *time.Timer) {
	this.receiveTimeoutTimer = timer
}

func (this *actorContextExtras) resetReceiveTimeoutTimer(time time.Duration) {
	this.receiveTimeoutTimer.Reset(time)
}

func (this *actorContextExtras) stopReceiveTimeoutTimer() {
	this.receiveTimeoutTimer.Stop()
}

func (this *actorContextExtras) killReceiveTimeoutTimer() {
	this.receiveTimeoutTimer.Stop()
	this.receiveTimeoutTimer = nil
}

func (this *actorContextExtras) addChild(pid *PID) {
	this.children.Add(pid)
}

func (this *actorContextExtras) removeChild(pid *PID) {
	this.children.Remove(pid)
}

func (this *actorContextExtras) watch(watcher *PID) {
	this.watchers.Add(watcher)
}

func (this *actorContextExtras) unwatch(watcher *PID) {
	this.watchers.Remove(watcher)
}

type actorContext struct {
	actor          Actor
	extras         *actorContextExtras
	props          *Props
	parent         *PID
	self           *PID
	receiveTimeout time.Duration
	supervisor     SupervisorStrategy
	producer       Producer
	//behavior       behaviorStack
	//receive        ActorFunc
	messageOrEnvelope interface{}
	state             contextState
}

func newActorContext(props *Props, parent *PID) *actorContext {
	this := &actorContext{
		parent: parent,
		props:  props,
	}

	this.incarnateActor()
	return this
}

func (ctx *actorContext) ensureExtras() *actorContextExtras {
	if ctx.extras == nil {
		ctxd := Context(ctx)
		if ctx.props != nil && ctx.props.contextDecoratorChain != nil {
			ctxd = ctx.props.contextDecoratorChain(ctxd)
		}
		ctx.extras = newActorContextExtras(ctxd)
	}
	return ctx.extras
}

func (ctx *actorContext) Actor() Actor {
	return ctx.actor
}

func (ctx *actorContext) Message() interface{} {
	return UnwrapEnvelopeMessage(ctx.messageOrEnvelope)
}

func (ctx *actorContext) Sender() *PID {
	return UnwrapEnvelopeSender(ctx.messageOrEnvelope)
}

func (ctx *actorContext) MessageHeader() ReadonlyMessageHeader {
	return UnwrapEnvelopeHeader(ctx.messageOrEnvelope)
}

func (ctx *actorContext) Send(pid *PID, message interface{}) {
	ctx.sendUserMessage(pid, message)
}

func (ctx *actorContext) Forward(pid *PID) {
	if msg, ok := ctx.messageOrEnvelope.(SystemMessage); ok {
		// SystemMessage cannot be forwarded
		plog.Error("SystemMessage cannot be forwarded", log.Message(msg))
		return
	}
	ctx.sendUserMessage(pid, ctx.messageOrEnvelope)
}

func (ctx *actorContext) sendUserMessage(pid *PID, message interface{}) {
	if ctx.props.senderMiddlewareChain != nil {
		ctx.props.senderMiddlewareChain(ctx.ensureExtras().context, pid, WrapEnvelope(message))
	} else {
		pid.sendUserMessage(message)
	}
}

func (ctx *actorContext) Request(pid *PID, message interface{}) {
	env := &MessageEnvelope{
		Header:  nil,
		Message: message,
		Sender:  ctx.Self(),
	}

	ctx.sendUserMessage(pid, env)
}

func (ctx *actorContext) RequestFuture(pid *PID, message interface{}, timeout time.Duration) *Future {
	future := NewFuture(timeout)
	env := &MessageEnvelope{
		Header:  nil,
		Message: message,
		Sender:  future.PID(),
	}
	ctx.sendUserMessage(pid, env)

	return future
}

func (ctx *actorContext) Stash() {
	extra := ctx.ensureExtras()
	if extra.stash == nil {
		extra.stash = linkedliststack.New()
	}
	extra.stash.Push(ctx.Message())
}

func (ctx *actorContext) cancelTimer() {
	if ctx.extras == nil || ctx.extras.receiveTimeoutTimer == nil {
		return
	}

	ctx.extras.killReceiveTimeoutTimer()
	ctx.receiveTimeout = 0
}

func (ctx *actorContext) receiveTimeoutHandler() {
	if ctx.extras != nil && ctx.extras.receiveTimeoutTimer != nil {
		ctx.cancelTimer()
		ctx.Send(ctx.self, receiveTimeoutMessage)
	}
}

func (ctx *actorContext) SetReceiveTimeout(d time.Duration) {
	if d <= 0 {
		panic("Duration must be greater than zero")
	}

	if d == ctx.receiveTimeout {
		return
	}

	if d < time.Millisecond {
		// anything less than than 1 millisecond is set to zero
		d = 0
	}

	ctx.receiveTimeout = d

	ctx.ensureExtras()
	ctx.extras.stopReceiveTimeoutTimer()
	if d > 0 {
		if ctx.extras.receiveTimeoutTimer == nil {
			ctx.extras.initReceiveTimeoutTimer(time.AfterFunc(d, ctx.receiveTimeoutHandler))
		} else {
			ctx.extras.resetReceiveTimeoutTimer(d)
		}
	}
}

func (ctx *actorContext) ReceiveTimeout() time.Duration {
	return ctx.receiveTimeout
}

func (ctx *actorContext) Children() []*PID {
	if ctx.extras == nil {
		return make([]*PID, 0)
	}

	r := make([]*PID, ctx.extras.children.Len())
	ctx.extras.children.ForEach(func(i int, p PID) {
		r[i] = &p
	})
	return r
}

func (ctx *actorContext) Self() *PID {
	return ctx.self
}

func (ctx *actorContext) Parent() *PID {
	return ctx.parent
}

func (ctx *actorContext) Receive(envelope *MessageEnvelope) {
	ctx.messageOrEnvelope = envelope
	ctx.defaultReceive()
	ctx.messageOrEnvelope = nil
}

func (ctx *actorContext) defaultReceive() {
	if _, ok := ctx.Message().(*PoisonPill); ok {
		ctx.self.Stop()
		return
	}

	//are we using decorators, if so, ensure it has been created
	if ctx.props.contextDecoratorChain != nil {
		ctx.actor.Receive(ctx.ensureExtras().context)
		return
	}

	ctx.actor.Receive(Context(ctx))
}

func (ctx *actorContext) EscalateFailure(reason interface{}, message interface{}) {
	failure := &Failure{Reason: reason, Who: ctx.self, RestartStats: ctx.ensureExtras().restartStats(), Message: message}
	ctx.self.sendSystemMessage(suspendMailboxMessage)
	if ctx.parent == nil {
		ctx.handleRootFailure(failure)
	} else {
		//TODO: Akka recursively suspends all children also on failure
		//Not sure if I think this is the right way to go, why do children need to wait for their parents failed state to recover?
		ctx.parent.sendSystemMessage(failure)
	}
}

func (ctx *actorContext) InvokeUserMessage(md interface{}) {
	if ctx.state == stateStopped {
		//already stopped
		return
	}

	influenceTimeout := true
	if ctx.receiveTimeout > 0 {
		_, influenceTimeout = md.(NotInfluenceReceiveTimeout)
		influenceTimeout = !influenceTimeout
		if influenceTimeout {
			ctx.extras.stopReceiveTimeoutTimer()
		}
	}

	ctx.processMessage(md)

	if ctx.receiveTimeout > 0 && influenceTimeout {
		ctx.extras.resetReceiveTimeoutTimer(ctx.receiveTimeout)
	}
}

func (ctx *actorContext) processMessage(m interface{}) {
	if ctx.props.receiverMiddlewareChain != nil {
		ctx.props.receiverMiddlewareChain(ctx.ensureExtras().context, WrapEnvelope(m))
		return
	}

	if ctx.props.contextDecoratorChain != nil {
		ctx.ensureExtras().context.Receive(WrapEnvelope(m))
		return
	}

	ctx.messageOrEnvelope = m
	ctx.defaultReceive()
	ctx.messageOrEnvelope = nil //release message
}

func (ctx *actorContext) incarnateActor() {
	ctx.state = stateAlive
	ctx.actor = ctx.props.producer()
}

func (ctx *actorContext) InvokeSystemMessage(message interface{}) {
	switch msg := message.(type) {
	case *continuation:
		ctx.messageOrEnvelope = msg.message // apply the message that was present when we started the await
		msg.f()                             // invoke the continuation in the current actor context
		ctx.messageOrEnvelope = nil         // release the message
	case *Started:
		ctx.InvokeUserMessage(msg) // forward
	case *Watch:
		ctx.handleWatch(msg)
	case *Unwatch:
		ctx.handleUnwatch(msg)
	case *Stop:
		ctx.handleStop(msg)
	case *Terminated:
		ctx.handleTerminated(msg)
	case *Failure:
		ctx.handleFailure(msg)
	case *Restart:
		ctx.handleRestart(msg)
	default:
		plog.Error("unknown system message", log.Message(msg))
	}
}

func (ctx *actorContext) handleRootFailure(failure *Failure) {
	defaultSupervisionStrategy.HandleFailure(ctx, failure.Who, failure.RestartStats, failure.Reason, failure.Message)
}

func (ctx *actorContext) handleWatch(msg *Watch) {
	if ctx.state >= stateStopping {
		msg.Watcher.sendSystemMessage(&Terminated{
			Who: ctx.self,
		})
	} else {
		ctx.ensureExtras().watch(msg.Watcher)
	}
}

func (ctx *actorContext) handleUnwatch(msg *Unwatch) {
	if ctx.extras == nil {
		return
	}
	ctx.extras.unwatch(msg.Watcher)
}

func (ctx *actorContext) handleRestart(msg *Restart) {
	ctx.state = stateRestarting
	ctx.InvokeUserMessage(restartingMessage)
	ctx.stopAllChildren()
	ctx.tryRestartOrTerminate()
}

//I am stopping
func (ctx *actorContext) handleStop(msg *Stop) {
	if ctx.state >= stateStopping {
		//already stopping or stopped
		return
	}

	ctx.state = stateStopping

	ctx.InvokeUserMessage(stoppingMessage)
	ctx.stopAllChildren()
	ctx.tryRestartOrTerminate()
}

//child stopped, check if we can stop or restart (if needed)
func (ctx *actorContext) handleTerminated(msg *Terminated) {
	if ctx.extras != nil {
		ctx.extras.removeChild(msg.Who)
	}

	ctx.InvokeUserMessage(msg)
	ctx.tryRestartOrTerminate()
}

//offload the supervision completely to the supervisor strategy
func (ctx *actorContext) handleFailure(msg *Failure) {
	if strategy, ok := ctx.actor.(SupervisorStrategy); ok {
		strategy.HandleFailure(ctx, msg.Who, msg.RestartStats, msg.Reason, msg.Message)
		return
	}
	ctx.supervisor.HandleFailure(ctx, msg.Who, msg.RestartStats, msg.Reason, msg.Message)
}

func (ctx *actorContext) stopAllChildren() {
	if ctx.extras == nil {
		return
	}
	ctx.extras.children.ForEach(func(_ int, pid PID) {
		pid.Stop()
	})
}

func (ctx *actorContext) tryRestartOrTerminate() {
	if ctx.extras != nil && !ctx.extras.children.Empty() {
		return
	}

	ctx.cancelTimer()

	switch ctx.state {
	case stateRestarting:
		ctx.restart()
	case stateStopping:
		ctx.finalizeStop()
	}
}

func (ctx *actorContext) restart() {
	ctx.incarnateActor()
	ctx.self.sendSystemMessage(resumeMailboxMessage)
	ctx.InvokeUserMessage(startedMessage)
	if ctx.extras != nil && ctx.extras.stash != nil {
		for !ctx.extras.stash.Empty() {
			msg, _ := ctx.extras.stash.Pop()
			ctx.InvokeUserMessage(msg)
		}
	}
}

func (ctx *actorContext) finalizeStop() {
	ProcessRegistry.Remove(ctx.self)
	ctx.InvokeUserMessage(stoppedMessage)
	otherStopped := &Terminated{Who: ctx.self}
	//Notify watchers
	if ctx.extras != nil {
		ctx.extras.watchers.ForEach(func(i int, pid PID) {
			pid.sendSystemMessage(otherStopped)
		})
	}
	//Notify parent
	if ctx.parent != nil {
		ctx.parent.sendSystemMessage(otherStopped)
	}
	ctx.state = stateStopped
}

func (ctx *actorContext) Watch(who *PID) {
	who.sendSystemMessage(&Watch{
		Watcher: ctx.self,
	})
}

func (ctx *actorContext) Unwatch(who *PID) {
	who.sendSystemMessage(&Unwatch{
		Watcher: ctx.self,
	})
}

func (ctx *actorContext) Respond(response interface{}) {
	// If the message is addressed to nil forward it to the dead letter channel
	if ctx.Sender() == nil {
		deadLetter.SendUserMessage(nil, response)
		return
	}

	ctx.Send(ctx.Sender(), response)
}

func (ctx *actorContext) Spawn(props *Props) *PID {
	pid, _ := ctx.SpawnNamed(props, ProcessRegistry.NextId())
	return pid
}

func (ctx *actorContext) SpawnPrefix(props *Props, prefix string) *PID {
	pid, _ := ctx.SpawnNamed(props, prefix+ProcessRegistry.NextId())
	return pid
}

func (ctx *actorContext) SpawnNamed(props *Props, name string) (*PID, error) {
	if props.guardianStrategy != nil {
		panic(errors.New("Props used to spawn child cannot have GuardianStrategy"))
	}

	pid, err := props.spawn(ctx.self.Id+"/"+name, ctx.self)
	if err != nil {
		return pid, err
	}

	ctx.ensureExtras().addChild(pid)

	return pid, nil
}

func (ctx *actorContext) GoString() string {
	return ctx.self.String()
}

func (ctx *actorContext) String() string {
	return ctx.self.String()
}

func (ctx *actorContext) AwaitFuture(f *Future, cont func(res interface{}, err error)) {
	wrapper := func() {
		cont(f.result, f.err)
	}

	message := ctx.messageOrEnvelope
	//invoke the callback when the future completes
	f.continueWith(func(res interface{}, err error) {
		//send the wrapped callaback as a continuation message to self
		ctx.self.sendSystemMessage(&continuation{
			f:       wrapper,
			message: message,
		})
	})
}

func (*actorContext) RestartChildren(pids ...*PID) {
	for _, pid := range pids {
		pid.sendSystemMessage(restartMessage)
	}
}

func (*actorContext) StopChildren(pids ...*PID) {
	for _, pid := range pids {
		pid.sendSystemMessage(stopMessage)
	}
}

func (*actorContext) ResumeChildren(pids ...*PID) {
	for _, pid := range pids {
		pid.sendSystemMessage(resumeMailboxMessage)
	}
}