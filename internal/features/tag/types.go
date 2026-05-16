package tag

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound      = errors.New("tag not found")
	ErrNameDuplicate = errors.New("tag name already exists")
)

type Tag struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateInput struct {
	ID   *uuid.UUID `json:"id,omitempty"`
	Name string     `json:"name" binding:"required,min=1,max=50"`
}

type UpdateInput struct {
	Name string `json:"name" binding:"required,min=1,max=50"`
}
