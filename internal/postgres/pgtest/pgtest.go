//go:build integration

// Package pgtest prepara la base de las pruebas de integración de un paquete. Cada paquete recibe una
// base propia, recién migrada: en CI los paquetes corren en paralelo contra el mismo servidor y no
// deben verse los datos ni las migraciones entre sí.
package pgtest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/DrewGGM/aluna/internal/postgres"
)

// Run es el cuerpo de TestMain: crea la base, deja en *pool un pool que se conecta como aluna_app
// (así una migración sin su GRANT falla en las pruebas y no en producción), corre las pruebas y borra
// la base. El servidor es ALUNA_TEST_DATABASE_URL (CI) o un PostgreSQL 16 de testcontainers.
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }
func Run(m *testing.M, pool **pgxpool.Pool) int {
	ctx := context.Background()
	dsn, cleanup, err := Start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgtest:", err)
		return 1
	}
	defer cleanup()
	if *pool, err = openAsApp(ctx, dsn); err != nil {
		fmt.Fprintln(os.Stderr, "pgtest:", err)
		return 1
	}
	defer (*pool).Close()
	return m.Run()
}

// Start crea una base propia ya migrada y devuelve su DSN, con el usuario dueño de las tablas (no
// aluna_app: quien la use no ejercita los GRANT), y la función que la borra. Es para pruebas que
// arrancan el binario o necesitan el DSN en vez de un pool.
func Start(ctx context.Context) (string, func(), error) {
	serverDSN, stop, err := server(ctx)
	if err != nil {
		return "", nil, err
	}
	db, err := createDatabase(ctx, serverDSN)
	if err != nil {
		stop()
		return "", nil, err
	}
	return db.dsn, func() { db.drop(ctx); stop() }, nil
}

func server(ctx context.Context) (string, func(), error) {
	if dsn := os.Getenv("ALUNA_TEST_DATABASE_URL"); dsn != "" {
		return dsn, func() {}, nil
	}
	container, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase("aluna_test"), tcpostgres.WithUsername("aluna"), tcpostgres.WithPassword("aluna"),
		tcpostgres.BasicWaitStrategies())
	if err != nil {
		return "", nil, fmt.Errorf("levantar PostgreSQL: %w", err)
	}
	stop := func() {
		if err := container.Terminate(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "pgtest: detener el contenedor (Ryuk lo recoge):", err)
		}
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		stop()
		return "", nil, fmt.Errorf("dirección de PostgreSQL: %w", err)
	}
	return dsn, stop, nil
}

type database struct {
	name, serverDSN, dsn string
}

// createDatabase crea una base con nombre aleatorio (solo [a-z2-7]: no admite inyección) y la migra.
func createDatabase(ctx context.Context, serverDSN string) (*database, error) {
	db := &database{name: "aluna_test_" + strings.ToLower(rand.Text()), serverDSN: serverDSN}
	var err error
	if db.dsn, err = withDatabase(serverDSN, db.name); err != nil {
		return nil, err
	}
	if err := db.exec(ctx, "CREATE DATABASE "+db.name); err != nil {
		return nil, fmt.Errorf("crear la base %s: %w", db.name, err)
	}
	if err := migrate(ctx, db.dsn); err != nil {
		db.drop(ctx)
		return nil, err
	}
	return db, nil
}

func openAsApp(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	appDSN, err := AppDSN(dsn)
	if err != nil {
		return nil, err
	}
	return postgres.Open(ctx, appDSN)
}

// AppDSN convierte el DSN del dueño en uno que se conecta con el rol aluna_app, como el proceso real.
func AppDSN(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errors.New("DSN de la base de prueba inválido")
	}
	query := u.Query()
	query.Set("options", "-c role=aluna_app")
	u.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20") // pgx no lee "+" como espacio
	return u.String(), nil
}

// drop borra la base. Un fallo se avisa: con un servidor compartido la base quedaría viva.
func (db *database) drop(ctx context.Context) {
	if err := db.exec(ctx, "DROP DATABASE IF EXISTS "+db.name+" WITH (FORCE)"); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: borrar la base %s: %v\n", db.name, err)
	}
}

func (db *database) exec(ctx context.Context, sql string) error {
	conn, err := pgx.Connect(ctx, db.serverDSN)
	if err != nil {
		return fmt.Errorf("conectar al servidor: %w", err)
	}
	_, err = conn.Exec(ctx, sql)
	return errors.Join(err, conn.Close(ctx))
}

// withDatabase cambia la base de un DSN postgres://. No incluye el DSN en el error: lleva la contraseña.
func withDatabase(serverDSN, name string) (string, error) {
	u, err := url.Parse(serverDSN)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("el DSN del servidor de prueba debe ser una URL postgres://")
	}
	u.Path = "/" + name
	return u.String(), nil
}

func migrate(ctx context.Context, dsn string) error {
	db, err := postgres.OpenForMigrations(dsn)
	if err != nil {
		return err
	}
	return errors.Join(postgres.Migrate(ctx, db, "up", io.Discard), db.Close())
}
