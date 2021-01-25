// Package service contains user related CRUD functionality.
package service

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/go-kit/kit/metrics"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/pkg/errors"
	"github.com/santiagoh1997/service-template/internal/business/auth"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrNotFound is used when a specific User is requested but does not exist.
	ErrNotFound = errors.New("not found")

	// ErrInvalidID occurs when an ID is not in a valid form.
	ErrInvalidID = errors.New("ID is not in its proper form")

	// ErrDuplicatedEmail is used whenever someone attempts to create a User
	// with an email that's already being used.
	ErrDuplicatedEmail = errors.New("email already in use")

	// ErrAuthenticationFailure occurs when a user attempts to authenticate but
	// anything goes wrong.
	ErrAuthenticationFailure = errors.New("authentication failed")

	// ErrForbidden occurs when a user tries to do something that is forbidden to them according to our access control policies.
	ErrForbidden = errors.New("attempted action is not allowed")
)

// UserService manages the set of API's for user access.
type UserService interface {
	Create(ctx context.Context, traceID string, nur NewUserRequest, now time.Time) (User, error)
	Update(ctx context.Context, traceID string, claims auth.Claims, userID string, uur UpdateUserRequest, now time.Time) (User, error)
	Delete(ctx context.Context, traceID string, claims auth.Claims, userID string) error
	GetAll(ctx context.Context, traceID string, pageNumber int, rowsPerPage int) ([]User, error)
	GetByID(ctx context.Context, traceID string, claims auth.Claims, userID string) (User, error)
	Authenticate(ctx context.Context, traceID string, now time.Time, email, password string) (auth.Claims, error)
}

type userService struct {
	db *sqlx.DB
}

// NewBasicService constructs a UserService for api access.
func NewBasicService(log *log.Logger, db *sqlx.DB) UserService {
	return userService{
		db: db,
	}
}

// New returns a UserService with instrumentation features.
func New(log *log.Logger, requestCount metrics.Counter, requestLatency metrics.Histogram, db *sqlx.DB) UserService {
	us := NewBasicService(log, db)
	us = NewInstrumentingDecorator(requestCount, requestLatency, us)

	return us
}

// Create creates a new user, generates a password hash,
// and saves it into the DB.
func (us userService) Create(ctx context.Context, traceID string, nur NewUserRequest, now time.Time) (User, error) {
	ctx, span := trace.SpanFromContext(ctx).Tracer().Start(ctx, "business.service.create")
	defer span.End()

	// Check if the email is already in use.
	const q1 = `SELECT COUNT(*) FROM users AS numUsers WHERE email=$1`

	var numUsers int
	if err := us.db.QueryRowContext(ctx, q1, nur.Email).Scan(&numUsers); err != nil {
		return User{}, errors.Wrapf(err, "looking for users with the email %s", nur.Email)
	}
	if numUsers != 0 {
		return User{}, ErrDuplicatedEmail
	}

	// Generate hash from passowrd.
	hash, err := bcrypt.GenerateFromPassword([]byte(nur.Password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, errors.Wrap(err, "generating password hash")
	}

	u := User{
		ID:           uuid.New().String(),
		Name:         nur.Name,
		LastName:     nur.LastName,
		Email:        nur.Email,
		Country:      nur.Country,
		PasswordHash: hash,
		Roles:        nur.Roles,
		DateCreated:  now.UTC(),
		DateUpdated:  now.UTC(),
	}

	const q = `INSERT INTO users
		(user_id, email, password_hash, roles, name, last_name, country, date_created, date_updated)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	if _, err := us.db.ExecContext(ctx, q, u.ID, u.Email, u.PasswordHash, u.Roles, u.Name, u.LastName, u.Country, u.DateCreated, u.DateUpdated); err != nil {
		return User{}, errors.Wrap(err, "inserting user")
	}

	return u, nil
}

// Update allows a client to update certain fields of a saved User.
func (us userService) Update(ctx context.Context, traceID string, claims auth.Claims, userID string, uur UpdateUserRequest, now time.Time) (User, error) {
	ctx, span := trace.SpanFromContext(ctx).Tracer().Start(ctx, "business.service.update")
	defer span.End()

	u, err := us.GetByID(ctx, traceID, claims, userID)
	if err != nil {
		return User{}, err
	}

	u.Name = uur.Name
	u.Country = uur.Country
	u.LastName = uur.LastName
	u.Email = uur.Email
	u.DateUpdated = now.UTC()

	const q = `
	UPDATE
		users
	SET
		"name" = $1,
		"last_name" = $2,
		"email" = $3,
		"country" = $4,
		"date_updated" = $5
	WHERE
		user_id=$6`

	if _, err = us.db.ExecContext(ctx, q, u.Name, u.LastName, u.Email, u.Country, u.DateUpdated, u.ID); err != nil {
		return User{}, errors.Wrap(err, "updating user")
	}

	return u, nil
}

// Delete deletes a User by its ID.
func (us userService) Delete(ctx context.Context, traceID string, claims auth.Claims, userID string) error {
	ctx, span := trace.SpanFromContext(ctx).Tracer().Start(ctx, "business.service.delete")
	defer span.End()

	if _, err := uuid.Parse(userID); err != nil {
		return ErrInvalidID
	}

	if !claims.Authorized(auth.RoleAdmin) && claims.Subject != userID {
		return ErrForbidden
	}

	const q = `
	DELETE FROM
		users
	WHERE
		user_id = $1`

	if _, err := us.db.ExecContext(ctx, q, userID); err != nil {
		return errors.Wrapf(err, "deleting user %s", userID)
	}

	return nil
}

// GetAll retrieves a list of existing users from the DB.
func (us userService) GetAll(ctx context.Context, traceID string, pageNumber int, rowsPerPage int) ([]User, error) {
	ctx, span := trace.SpanFromContext(ctx).Tracer().Start(ctx, "business.service.getAll")
	defer span.End()

	const q = `
	SELECT
		*
	FROM
		users
	ORDER BY
		user_id
	OFFSET $1 ROWS FETCH NEXT $2 ROWS ONLY`

	offset := (pageNumber - 1) * rowsPerPage

	users := []User{}
	if err := us.db.SelectContext(ctx, &users, q, offset, rowsPerPage); err != nil {
		return nil, errors.Wrap(err, "selecting users")
	}

	return users, nil
}

// GetByID retrieves a User from the DB by its ID.
func (us userService) GetByID(ctx context.Context, traceID string, claims auth.Claims, userID string) (User, error) {
	ctx, span := trace.SpanFromContext(ctx).Tracer().Start(ctx, "business.service.getbyid")
	defer span.End()

	if _, err := uuid.Parse(userID); err != nil {
		return User{}, ErrInvalidID
	}

	if !claims.Authorized(auth.RoleAdmin) && claims.Subject != userID {
		return User{}, ErrForbidden
	}

	const q = `SELECT * FROM users WHERE user_id = $1`

	var u User
	if err := us.db.GetContext(ctx, &u, q, userID); err != nil {
		if err == sql.ErrNoRows {
			return User{}, ErrNotFound
		}
		return User{}, errors.Wrapf(err, "selecting user %q", userID)
	}

	return u, nil
}

// Authenticate finds a user by their email and verifies their password. On
// success it returns a Claims representing the user. The claims can be
// used to generate a token for future authentication.
func (us userService) Authenticate(ctx context.Context, traceID string, now time.Time, email, password string) (auth.Claims, error) {
	ctx, span := trace.SpanFromContext(ctx).Tracer().Start(ctx, "business.service.authenticate")
	defer span.End()

	u, err := us.getByEmail(ctx, traceID, email)
	if err != nil {
		if err == ErrNotFound {
			return auth.Claims{}, ErrAuthenticationFailure
		}
		return auth.Claims{}, errors.Wrap(err, "selecting single user")
	}

	if err := bcrypt.CompareHashAndPassword(u.PasswordHash, []byte(password)); err != nil {
		return auth.Claims{}, ErrAuthenticationFailure
	}

	claims := auth.Claims{
		// TODO: Customize claims to suit the project.
		StandardClaims: jwt.StandardClaims{
			Issuer:    "service template",
			Subject:   u.ID,
			Audience:  "clients",
			ExpiresAt: now.Add(time.Hour).Unix(),
			IssuedAt:  now.Unix(),
		},
		Roles: u.Roles,
	}

	return claims, nil
}

// getByEmail retrieves a User in the DB by its email.
func (us userService) getByEmail(ctx context.Context, traceID string, email string) (User, error) {
	ctx, span := trace.SpanFromContext(ctx).Tracer().Start(ctx, "business.service.getByEmail")
	defer span.End()

	const q = `SELECT * FROM users WHERE email = $1`

	var u User
	if err := us.db.GetContext(ctx, &u, q, email); err != nil {
		if err == sql.ErrNoRows {
			return User{}, ErrNotFound
		}
		return User{}, errors.Wrapf(err, "selecting user %q", email)
	}

	return u, nil
}
