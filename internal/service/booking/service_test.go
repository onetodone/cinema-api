package booking

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/payment"
)

var (
	testNow = time.Date(2030, 1, 10, 9, 0, 0, 0, time.UTC)
	cinema  = time.FixedZone("CET", 3600)
	ann     = uuid.NewV7()
	bob     = uuid.NewV7()
)

const (
	holdTTL        = 15 * time.Minute
	paymentTimeout = 10 * time.Second
	paymentGrace   = 2 * time.Minute
)

// newTestService returns a service over a fake database with three showtimes:
//   - 1: scheduled, seats 1-4 (seat 4 is VIP);
//   - 2: scheduled but already started, seat 10;
//   - 3: canceled, seat 20.
func newTestService(t *testing.T) (*Service, *memDB) {
	t.Helper()
	svc, db, _ := newPayTestService(t)
	return svc, db
}

// newPayTestService is newTestService with the payment provider "card", whose answers the test scripts. By
// default it charges every payment. A second provider, "old", is registered but takes no new payments.
func newPayTestService(t *testing.T) (*Service, *memDB, *fakeProvider) {
	t.Helper()
	db := newMemDB(testNow)
	st := func(id int64, status domain.ShowtimeStatus) domain.ShowtimeRef {
		return domain.ShowtimeRef{
			ID: id, Status: status, StartsAt: time.Date(2030, 1, 10, 19, 0, 0, 0, time.UTC),
			Movie: domain.MovieSummary{ID: 1, Title: "Dune"}, Hall: domain.Hall{ID: 1, Name: "Hall 1"},
		}
	}
	db.addShowtime(st(1, domain.ShowtimeScheduled), false, 1, 1000, 1000, 1000, 1500)
	db.addShowtime(st(2, domain.ShowtimeScheduled), true, 10, 1000)
	db.addShowtime(st(3, domain.ShowtimeCanceled), false, 20, 1000)

	card := &fakeProvider{id: "card"}
	providers := payment.NewRegistry()
	for _, reg := range []struct {
		p       payment.Provider
		enabled bool
	}{{card, true}, {&fakeProvider{id: "old"}, false}} {
		if err := providers.Register(reg.p, reg.enabled); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		Location: cinema, Currency: "EUR", HoldTTL: holdTTL, MaxSeats: 4,
		PaymentTimeout: paymentTimeout, PaymentGrace: paymentGrace,
	}
	return New(db, db, providers, cfg, slog.New(slog.DiscardHandler)), db, card
}

func code(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func TestCreateHoldsSeats(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)

	b, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{4, 1, 2}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if b.Status != domain.BookingPending || b.UserID != ann || b.Showtime.ID != 1 || b.TotalCents != 3500 {
		t.Errorf("booking = %+v", b)
	}
	if !b.ExpiresAt.Equal(testNow.Add(holdTTL)) {
		t.Errorf("ExpiresAt = %s, want %s", b.ExpiresAt, testNow.Add(holdTTL))
	}
	if b.Showtime.StartsAt.Location() != cinema {
		t.Errorf("showtime start is in %s, want the cinema zone", b.Showtime.StartsAt.Location())
	}
	var order []int64
	for _, s := range b.Seats {
		order = append(order, s.SeatID)
	}
	if !slices.Equal(order, []int64{1, 2, 4}) { // A1, B2, B4: seat map order
		t.Errorf("seats in order %v, want [1 2 4]", order)
	}

	for _, id := range []int64{1, 2, 4} {
		if s := db.seat(1, id); s.seat.Status != domain.SeatHeld || s.bookingID != b.ID {
			t.Errorf("seat %d = %+v, want held by the booking", id, s)
		}
	}
	if s := db.seat(1, 3); s.seat.Status != domain.SeatAvailable {
		t.Errorf("seat 3 = %s, want available", s.seat.Status)
	}
	want := []string{"lock seats [1 2 4]", "insert booking", "hold seats [1 2 4]"}
	if !slices.Equal(db.calls, want) {
		t.Errorf("calls = %q, want %q (seats locked in id order before anything changes)", db.calls, want)
	}
}

func TestCreateRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   domain.NewBooking
		code string
	}{
		{name: "invalid input", in: domain.NewBooking{ShowtimeID: 1}, code: ""},
		{name: "too many seats", in: domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 2, 3, 4, 5}}, code: ""},
		{name: "unknown showtime", in: domain.NewBooking{ShowtimeID: 99, SeatIDs: []int64{1}}, code: domain.CodeShowtimeNotFound},
		{name: "started showtime", in: domain.NewBooking{ShowtimeID: 2, SeatIDs: []int64{10}}, code: domain.CodeShowtimeNotBookable},
		{name: "canceled showtime", in: domain.NewBooking{ShowtimeID: 3, SeatIDs: []int64{20}}, code: domain.CodeShowtimeNotBookable},
		{name: "seat of another showtime", in: domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 10}}, code: domain.CodeUnknownSeat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, db := newTestService(t)

			_, err := svc.Create(t.Context(), ann, tt.in)
			if err == nil {
				t.Fatal("Create succeeded")
			}
			if tt.code == "" {
				var ve *domain.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("err = %v, want a validation error", err)
				}
			} else if code(err) != tt.code {
				t.Errorf("err = %v, want code %s", err, tt.code)
			}
			if len(db.state.bookings) != 0 {
				t.Error("a rejected request left a booking behind")
			}
		})
	}
}

func TestCreateReportsEveryTakenSeatAndHoldsNothing(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)

	first, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{2, 3}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Create(t.Context(), bob, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{3, 1, 2}})
	var taken *domain.SeatsUnavailableError
	if !errors.As(err, &taken) || !slices.Equal(taken.SeatIDs, []int64{2, 3}) {
		t.Fatalf("err = %v, want seats 2 and 3 unavailable", err)
	}
	if code(err) != domain.CodeSeatUnavailable {
		t.Errorf("code = %q", code(err))
	}
	if s := db.seat(1, 1); s.seat.Status != domain.SeatAvailable {
		t.Errorf("seat 1 = %s: an all-or-nothing request held part of its seats", s.seat.Status)
	}
	if s := db.seat(1, 2); s.bookingID != first.ID {
		t.Errorf("seat 2 changed hands")
	}
}

func TestCreateAllowsOneActiveBookingPerShowtime(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)

	first, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{2}})
	if code(err) != domain.CodeActiveBookingExists {
		t.Fatalf("second booking: %v, want ACTIVE_BOOKING_EXISTS", err)
	}
	if s := db.seat(1, 2); s.seat.Status != domain.SeatAvailable {
		t.Errorf("seat 2 = %s after the rejected booking", s.seat.Status)
	}

	// After a cancel the user may book again.
	if err := svc.Cancel(t.Context(), ann, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{2}}); err != nil {
		t.Errorf("booking after cancel: %v", err)
	}
}

func TestCreateRollsBackWhenTheHoldIsIncomplete(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)
	db.holdShortfall = 1

	_, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 2}})
	if err == nil || code(err) != "" {
		t.Fatalf("err = %v, want an internal error", err)
	}
	if len(db.state.bookings) != 0 || db.seat(1, 1).seat.Status != domain.SeatAvailable {
		t.Error("the failed transaction was not rolled back")
	}
}

func TestGetShowsOnlyOwnBookings(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	created, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(t.Context(), ann, created.ID)
	if err != nil || got.ID != created.ID || got.Showtime.StartsAt.Location() != cinema {
		t.Errorf("Get own booking = %+v, %v", got, err)
	}
	if _, err := svc.Get(t.Context(), bob, created.ID); code(err) != domain.CodeBookingNotFound {
		t.Errorf("Get another user's booking: %v, want BOOKING_NOT_FOUND", err)
	}
}

func TestListPagesNewestFirst(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)
	// Three bookings of ann in three showtimes that accept bookings.
	for id := int64(4); id <= 6; id++ {
		db.addShowtime(domain.ShowtimeRef{ID: id, Status: domain.ShowtimeScheduled}, false, id*10, 1000)
	}
	var ids []uuid.UUID
	for id := int64(4); id <= 6; id++ {
		b, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: id, SeatIDs: []int64{id * 10}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	if _, err := svc.Create(t.Context(), bob, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1}}); err != nil {
		t.Fatal(err)
	}

	page1, err := svc.List(t.Context(), ann, uuid.UUID{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Bookings) != 2 || page1.Bookings[0].ID != ids[2] || page1.Bookings[1].ID != ids[1] {
		t.Fatalf("page 1 = %v", page1.Bookings)
	}
	if page1.NextBeforeID != ids[1] {
		t.Errorf("next = %s, want %s", page1.NextBeforeID, ids[1])
	}

	page2, err := svc.List(t.Context(), ann, page1.NextBeforeID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Bookings) != 1 || page2.Bookings[0].ID != ids[0] || page2.NextBeforeID != (uuid.UUID{}) {
		t.Errorf("page 2 = %+v", page2)
	}

	for limit, want := range map[int]int{0: DefaultPageSize + 1, -3: DefaultPageSize + 1, 500: MaxPageSize + 1} {
		if _, err := svc.List(t.Context(), ann, uuid.UUID{}, limit); err != nil || db.listLimit != want {
			t.Errorf("List(limit %d) asked the repository for %d rows, want %d (err %v)", limit, db.listLimit, want, err)
		}
	}
}

func TestCancelReleasesSeats(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)

	b, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Cancel(t.Context(), ann, b.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if got, _ := db.booking(b.ID); got.Status != domain.BookingCanceled {
		t.Errorf("status = %s, want canceled", got.Status)
	}
	for _, id := range []int64{1, 2} {
		if s := db.seat(1, id); s.seat.Status != domain.SeatAvailable || s.bookingID != (uuid.UUID{}) {
			t.Errorf("seat %d = %+v, want available", id, s)
		}
	}
	want := []string{"lock booking", "lock seats of 1 bookings", "release seats", "set status canceled"}
	if !slices.Equal(db.calls, want) {
		t.Errorf("calls = %q, want %q (booking locked before its seats)", db.calls, want)
	}

	// Canceling again changes nothing and succeeds.
	if err := svc.Cancel(t.Context(), ann, b.ID); err != nil {
		t.Errorf("second cancel: %v", err)
	}
	if !slices.Equal(db.calls, []string{"lock booking"}) {
		t.Errorf("second cancel calls = %q, want only the booking lock", db.calls)
	}
}

func TestCancelByStatus(t *testing.T) {
	t.Parallel()

	for status, wantCode := range map[domain.BookingStatus]string{
		domain.BookingExpired:    "",
		domain.BookingCanceled:   "",
		domain.BookingProcessing: domain.CodeBookingNotCancelable,
		domain.BookingPaid:       domain.CodeBookingNotCancelable,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			svc, db := newTestService(t)
			b, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1}})
			if err != nil {
				t.Fatal(err)
			}
			db.setBookingStatus(b.ID, status)

			err = svc.Cancel(t.Context(), ann, b.ID)
			if code(err) != wantCode || (wantCode == "" && err != nil) {
				t.Errorf("Cancel of a %s booking: %v, want code %q", status, err, wantCode)
			}
			if got, _ := db.booking(b.ID); got.Status != status {
				t.Errorf("status changed to %s", got.Status)
			}
		})
	}
}

func TestCancelRejectsOtherUsersAndInconsistentReleases(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)
	b, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Cancel(t.Context(), bob, b.ID); code(err) != domain.CodeBookingNotFound {
		t.Errorf("cancel by another user: %v, want BOOKING_NOT_FOUND", err)
	}

	db.releaseExtra = 1
	if err := svc.Cancel(t.Context(), ann, b.ID); err == nil || code(err) != "" {
		t.Errorf("cancel with a release count mismatch: %v, want an internal error", err)
	}
	if got, _ := db.booking(b.ID); got.Status != domain.BookingPending || db.seat(1, 1).seat.Status != domain.SeatHeld {
		t.Error("the failed cancel was not rolled back")
	}
}

func TestExpireBatchExpiresDueBookingsEarliestFirst(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)
	ctx := t.Context()
	cat := uuid.NewV7()

	create := func(user uuid.UUID, seats ...int64) domain.Booking {
		t.Helper()
		b, err := svc.Create(ctx, user, domain.NewBooking{ShowtimeID: 1, SeatIDs: seats})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first := create(ann, 1, 2) // due at +15m
	db.advance(time.Minute)
	second := create(bob, 3) // due at +16m
	db.advance(time.Minute)
	notDue := create(cat, 4) // due at +17m
	db.advance(holdTTL - time.Minute)

	// The clock is at +16m: the first two are due, the second exactly at its deadline. A batch of one takes the
	// earliest deadline.
	batch, err := svc.ExpireBatch(ctx, 1)
	if err != nil {
		t.Fatalf("ExpireBatch: %v", err)
	}
	if !slices.Equal(batch.BookingIDs, []uuid.UUID{first.ID}) || batch.Seats != 2 {
		t.Errorf("first batch = %+v, want ann's booking with 2 seats", batch)
	}
	want := []string{"lock expired bookings", "lock seats of 1 bookings", "release seats", "set status expired"}
	if !slices.Equal(db.calls, want) {
		t.Errorf("calls = %q, want %q (bookings locked before their seats)", db.calls, want)
	}

	batch, err = svc.ExpireBatch(ctx, 10)
	if err != nil || !slices.Equal(batch.BookingIDs, []uuid.UUID{second.ID}) || batch.Seats != 1 {
		t.Errorf("second batch = %+v, %v; want bob's booking with 1 seat", batch, err)
	}
	batch, err = svc.ExpireBatch(ctx, 10)
	if err != nil || len(batch.BookingIDs) != 0 || batch.Seats != 0 {
		t.Errorf("third batch = %+v, %v; want nothing left to expire", batch, err)
	}
	if !slices.Equal(db.calls, []string{"lock expired bookings"}) {
		t.Errorf("empty batch calls = %q, want only the claim", db.calls)
	}

	for id, status := range map[uuid.UUID]domain.BookingStatus{
		first.ID: domain.BookingExpired, second.ID: domain.BookingExpired, notDue.ID: domain.BookingPending,
	} {
		if got, _ := db.booking(id); got.Status != status {
			t.Errorf("booking %s is %s, want %s", id, got.Status, status)
		}
	}
	for _, id := range []int64{1, 2, 3} {
		if s := db.seat(1, id); s.seat.Status != domain.SeatAvailable || s.bookingID != (uuid.UUID{}) {
			t.Errorf("seat %d = %+v, want available", id, s)
		}
	}
	if s := db.seat(1, 4); s.seat.Status != domain.SeatHeld || s.bookingID != notDue.ID {
		t.Errorf("seat 4 = %+v, want still held by cat", s)
	}

	// The expired booking no longer counts as ann's active booking, and its seats can be booked again.
	if _, err := svc.Create(ctx, ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1, 2}}); err != nil {
		t.Errorf("booking again after expiry: %v", err)
	}
}

func TestExpireBatchRollsBackAnInconsistentRelease(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)
	b, err := svc.Create(t.Context(), ann, domain.NewBooking{ShowtimeID: 1, SeatIDs: []int64{1}})
	if err != nil {
		t.Fatal(err)
	}
	db.advance(holdTTL)

	db.releaseExtra = 1
	if batch, err := svc.ExpireBatch(t.Context(), 10); err == nil || code(err) != "" || len(batch.BookingIDs) != 0 {
		t.Errorf("ExpireBatch with a release count mismatch = %+v, %v; want an internal error", batch, err)
	}
	if got, _ := db.booking(b.ID); got.Status != domain.BookingPending || db.seat(1, 1).seat.Status != domain.SeatHeld {
		t.Error("the failed batch was not rolled back")
	}
}

func TestExpireBatchRejectsANonPositiveLimit(t *testing.T) {
	t.Parallel()
	svc, db := newTestService(t)

	for _, limit := range []int{0, -1} {
		if _, err := svc.ExpireBatch(t.Context(), limit); err == nil {
			t.Errorf("ExpireBatch(%d) succeeded", limit)
		}
	}
	if len(db.calls) != 0 {
		t.Errorf("calls = %q, want none", db.calls)
	}
}
