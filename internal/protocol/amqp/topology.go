// Package amqp mocks AsyncAPI operations on RabbitMQ.
package amqp

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/mockmint/mockmint/internal/pkg"
	"github.com/mockmint/mockmint/internal/spec/asyncapi"
)

// ExchangeDecl declares an exchange.
type ExchangeDecl struct {
	Name       string
	Kind       string
	Durable    bool
	AutoDelete bool
	// Passive only checks that the exchange exists (mockmint.yaml exchange
	// overrides name exchanges mockmint does not own).
	Passive bool
	owner   string
}

// QueueDecl declares a queue.
type QueueDecl struct {
	Name       string
	Durable    bool
	AutoDelete bool
	Exclusive  bool
	DeadLetter string // x-dead-letter-exchange, "" for none
	owner      string
}

// BindingDecl binds a queue to an exchange.
type BindingDecl struct {
	Queue, Exchange, Key string
}

// Topology is everything mockmint declares on connect, deduplicated and
// sorted by name.
type Topology struct {
	Exchanges []ExchangeDecl
	Queues    []QueueDecl
	Bindings  []BindingDecl
}

// Plan computes the topology for packages that have declaration enabled.
// Declaring the same exchange or queue twice with different settings is an
// error naming both owners, rather than a PRECONDITION_FAILED at runtime.
func Plan(pkgs []*pkg.Package) (*Topology, error) {
	p := &planner{exchanges: map[string]ExchangeDecl{}, queues: map[string]QueueDecl{}, bindings: map[BindingDecl]bool{}}
	for _, pk := range pkgs {
		if pk.Async == nil || !pk.Async.Declare {
			continue
		}
		dlx := ""
		if dl := pk.Async.DeadLetter; dl != nil {
			dlx = dl.Exchange
			owner := pk.Name + " deadLetter"
			p.exchange(ExchangeDecl{Name: dl.Exchange, Kind: amqp.ExchangeFanout, Durable: true, owner: owner})
			p.queue(QueueDecl{Name: dl.Queue, Durable: true, owner: owner})
			p.bindings[BindingDecl{Queue: dl.Queue, Exchange: dl.Exchange}] = true
		}
		for _, op := range pk.Async.Operations {
			owner := pk.Name + " " + op.ID
			switch {
			case op.Action == asyncapi.ActionReceive:
				p.queue(queueDecl(op.Channel, op.Queue, dlx, owner))
				if op.BindExchange != "" {
					p.exchange(exchangeDecl(op.Channel, owner))
					p.bindings[BindingDecl{Queue: op.Queue, Exchange: op.BindExchange, Key: op.BindKey}] = true
				}
				if rs := op.Replies; rs != nil && rs.Channel != nil && rs.Exchange != "" {
					p.exchange(exchangeDecl(rs.Channel, owner+" reply"))
				}
			case op.ExchangeOverride && op.Exchange != "":
				p.exchange(ExchangeDecl{Name: op.Exchange, Passive: true, owner: owner + " exchange override"})
			case op.Exchange != "":
				p.exchange(exchangeDecl(op.Channel, owner))
			case op.RoutingKey != "":
				// Default exchange: the routing key is a queue; declare it so
				// published messages are kept.
				p.queue(queueDecl(op.Channel, op.RoutingKey, "", owner))
			}
		}
	}
	if err := errors.Join(p.errs...); err != nil {
		return nil, err
	}
	return p.topology(), nil
}

type planner struct {
	exchanges map[string]ExchangeDecl
	queues    map[string]QueueDecl
	bindings  map[BindingDecl]bool
	errs      []error
}

func (p *planner) exchange(e ExchangeDecl) {
	if prev, ok := p.exchanges[e.Name]; ok {
		switch {
		case e.Passive:
			return // an existence check adds nothing to a declaration
		case prev.Passive:
			p.exchanges[e.Name] = e // a real declaration supersedes the check
			return
		}
		if prev.Kind != e.Kind || prev.Durable != e.Durable || prev.AutoDelete != e.AutoDelete {
			p.errs = append(p.errs, fmt.Errorf("exchange %q is declared differently by %s (%s) and %s (%s)",
				e.Name, prev.owner, describeExchange(prev), e.owner, describeExchange(e)))
		}
		return
	}
	p.exchanges[e.Name] = e
}

func (p *planner) queue(q QueueDecl) {
	if prev, ok := p.queues[q.Name]; ok {
		if prev.Durable != q.Durable || prev.AutoDelete != q.AutoDelete || prev.Exclusive != q.Exclusive || prev.DeadLetter != q.DeadLetter {
			p.errs = append(p.errs, fmt.Errorf("queue %q is declared differently by %s (%s) and %s (%s)",
				q.Name, prev.owner, describeQueue(prev), q.owner, describeQueue(q)))
		}
		return
	}
	p.queues[q.Name] = q
}

func (p *planner) topology() *Topology {
	t := &Topology{}
	for _, e := range p.exchanges {
		t.Exchanges = append(t.Exchanges, e)
	}
	for _, q := range p.queues {
		t.Queues = append(t.Queues, q)
	}
	for b := range p.bindings {
		t.Bindings = append(t.Bindings, b)
	}
	slices.SortFunc(t.Exchanges, func(a, b ExchangeDecl) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(t.Queues, func(a, b QueueDecl) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(t.Bindings, func(a, b BindingDecl) int {
		return cmp.Or(cmp.Compare(a.Queue, b.Queue), cmp.Compare(a.Exchange, b.Exchange), cmp.Compare(a.Key, b.Key))
	})
	return t
}

func exchangeDecl(ch *asyncapi.Channel, owner string) ExchangeDecl {
	e := ch.AMQP.Exchange
	return ExchangeDecl{
		Name:       e.Name,
		Kind:       cmp.Or(e.Type, amqp.ExchangeTopic),
		Durable:    e.Durable == nil || *e.Durable,
		AutoDelete: e.AutoDelete,
		owner:      owner,
	}
}

// queueDecl declares name with the channel's queue settings when the
// channel names that queue; mockmint-generated queues are transient.
func queueDecl(ch *asyncapi.Channel, name, dlx, owner string) QueueDecl {
	b := ch.AMQP.Queue
	if strings.HasPrefix(name, "mockmint.") && b.Name == "" {
		return QueueDecl{Name: name, AutoDelete: true, DeadLetter: dlx, owner: owner}
	}
	return QueueDecl{
		Name:       name,
		Durable:    b.Durable == nil || *b.Durable,
		AutoDelete: b.AutoDelete,
		Exclusive:  b.Exclusive,
		DeadLetter: dlx,
		owner:      owner,
	}
}

func describeExchange(e ExchangeDecl) string {
	return fmt.Sprintf("%s durable=%v autoDelete=%v", e.Kind, e.Durable, e.AutoDelete)
}

func describeQueue(q QueueDecl) string {
	return fmt.Sprintf("durable=%v autoDelete=%v exclusive=%v deadLetter=%q", q.Durable, q.AutoDelete, q.Exclusive, q.DeadLetter)
}

// declarer is the subset of *amqp.Channel that Apply uses.
type declarer interface {
	ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error
	ExchangeDeclarePassive(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error
	QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error)
	QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error
}

// Apply declares t on ch: exchanges, then queues, then bindings.
func (t *Topology) Apply(ch declarer) error {
	for _, e := range t.Exchanges {
		if e.Passive {
			if err := ch.ExchangeDeclarePassive(e.Name, "", false, false, false, false, nil); err != nil {
				return fmt.Errorf("exchange %q (%s) does not exist: %w", e.Name, e.owner, err)
			}
			continue
		}
		if err := ch.ExchangeDeclare(e.Name, e.Kind, e.Durable, e.AutoDelete, false, false, nil); err != nil {
			return fmt.Errorf("declare exchange %q: %w", e.Name, err)
		}
	}
	for _, q := range t.Queues {
		var args amqp.Table
		if q.DeadLetter != "" {
			args = amqp.Table{"x-dead-letter-exchange": q.DeadLetter}
		}
		if _, err := ch.QueueDeclare(q.Name, q.Durable, q.AutoDelete, q.Exclusive, false, args); err != nil {
			return fmt.Errorf("declare queue %q: %w", q.Name, err)
		}
	}
	for _, b := range t.Bindings {
		if err := ch.QueueBind(b.Queue, b.Key, b.Exchange, false, nil); err != nil {
			return fmt.Errorf("bind queue %q to %q (%q): %w", b.Queue, b.Exchange, b.Key, err)
		}
	}
	return nil
}
