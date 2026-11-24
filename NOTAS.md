# NOTAS — `--migrate` (A0: reconocimiento)

Rama: `migracion-de-archivos` (creada desde `master` @ 8f4873d5d).
Baseline: `go build ./...` OK, `go test ./internal/fs/... ./internal/archiver/...` verde (go1.26.10).

## Decisiones del §3 (validadas contra el código)

### 1. Interfaz exacta `fs.FS` / `fs.File` (`internal/fs/interface.go`)

```go
type FS interface {
    OpenFile(name string, flag int, metadataOnly bool) (File, error) // solo O_RDONLY|O_NOFOLLOW|O_DIRECTORY
    Lstat(name string) (*ExtendedFileInfo, error)
    Join(elem ...string) string
    Separator() string
    Abs(path string) (string, error)
    Clean(path string) string
    VolumeName(path string) string
    IsAbs(path string) bool
    Dir(path string) string
    Base(path string) string
}

type File interface {
    MakeReadable() error
    io.Reader
    io.Closer
    Readdirnames(n int) ([]string, error)
    Stat() (*ExtendedFileInfo, error)
    ToNode(ignoreXattrListError bool, warnf func(format string, args ...any)) (*data.Node, error)
}
```

- Confirmado: `File` **exige `ToNode`**. El `File` del zip construye el `data.Node`
  él mismo desde el header del zip (nombre, tipo, modo, mtime, tamaño, `LinkTarget`).
- Referencia de implementación: `fs_reader.go` (`reader`, `readerFile`, `fakeFile`,
  `fakeDir`, `pathError`). `ToNode` de referencia: `buildBasicNode(path, fi)` + uid/gid + ChangeTime.
- `fs.Readdirnames(filesys, dir, flags)` abre con `O_RDONLY|O_DIRECTORY|O_NOFOLLOW` y
  `metadataOnly=false`, y llama `Readdirnames(-1)`. El `Zip.OpenFile` debe aceptar esas
  flags en directorios.
- Metadatos: `data.Node` usa `ModTime/AccessTime/ChangeTime`, `Mode`, `Size` (solo ficheros
  regulares, ver `buildBasicNode`), `LinkTarget`/`LinkTargetRaw` para symlinks.

### 2. Target `/` (Opción A) — funciona, sin tocar el archiver

`resolveRelativeTargets` (archiver.go:794) expande cualquier target sin componentes de
path (p. ej. `/`) con `fs.Readdirnames(filesys, target, O_NOFOLLOW)` y crea un target por
cada entrada de primer nivel con `Explicit: false` (los excludes siguen aplicando).
Por tanto `targets = []string{"/"}` + `targetFS = zipFS` es suficiente: el snapshot queda
con los paths internos del zip tal cual (`/docs/readme.txt`, ...). Opción A y B convirgen;
no hace falta modificar `internal/archiver`.

### 3. Consistencia "fichero cambiado durante la lectura"

Flujo del archiver (`save()` → `fileSaver.saveFile`):
`OpenFile(name, O_NOFOLLOW, metadataOnly=true)` → `meta.Stat()` → (sin padre: sin
`fileChanged`) → `meta.MakeReadable()` → `meta.Stat()` otra vez → `meta.ToNode()`.
En `Zip` todas las llamadas derivan del mismo índice inmemoria del directorio central
(inmutable tras `NewZip`), así que `Lstat`/`Stat`/`ToNode` son coherentes por
construcción. No hay "cambio" posible: el `.zip` se abre una vez con `zip.OpenReader`.

### 4. `--parent` / detección de cambios

Con `--migrate` se fuerza `opts.Force = true` **antes** de `findParentSnapshot`, que con
`Force` devuelve `nil` sin consultar el repo → siempre "no parent snapshot", como pide el
§4.3. `--ignore-inode`/`--ignore-ctime` resultan irrelevantes (no hay nodos anteriores
comparables). La dedup por contenido de restic evita reescribir datos repetidos.

## Notas de implementación

- `zip.FileHeader.Mode()` (stdlib) ya decodifica los bits unix (permisos + tipo, incl.
  `ModeSymlink`) vía `unixModeToFileMode`; para creador MS-DOS devuelve 0666/0777|Dir.
  Según §4.4: modo solo si creador Unix (o macOS); si no, 0644 ficheros / 0755 dirs.
- Symlinks del zip: el target se lee al indexar (máx. 4096 B; si sobrepasa, se trata la
  entrada como fichero regular + warning). `ToNode` rellena `LinkTarget` (o
  `LinkTargetRaw` si no es UTF-8 válido).
- Nombres no-UTF-8: sin el flag UTF-8 (bit 11), el nombre se decodifica como CP437
  y se convierte a UTF-8. Con el flag, se usa tal cual. Nota: en
  `golang.org/x/text v0.43.0` ya no existe `charmap.CP437`, así que la tabla
  de 0x80–0xFF (`cp437High`, 128 runes) va embebida en `fs_zip.go` vía
  `cp437Decode`; verificada contra `python3 bytes.decode('cp437')` y
  glibc `iconv -f CP437` (coinciden; p. ej. 0xE9→Θ U+0398, 0x82→é).
  El reader de `archive/zip` (Go 1.26) ya deduce `FileHeader.NonUTF8` del
  flag 0x800; el writer solo setea el flag UTF-8 si el nombre es UTF-8
  válido y multibyte (un byte 0x82 solo no lo setea → `NonUTF8=true` al leer).
- Warnings: desviación menor del contrato §2 — `NewZip(zipPath string, warnf func(string,
  ...any))` en lugar de `NewZip(zipPath string)`, para emitir los warnings del §5 por el
  mismo canal que el resto del comando (`printer.E`). Sin `warnf` no hay forma de
  reportar entradas omitidas.
- Concurrency: cada `OpenFile` crea un `zipFile` nuevo; `MakeReadable` abre un
  `zip.File.Open()` independiente (lectura vía `io.ReaderAt` del `*os.File` subyacente),
  sin estado mutable compartido → seguro para `ReadConcurrency` > 1.

## Resumen por fase

### A1 — ZipFS núcleo (commit `d12bf91d3`)

- Qué hice: `internal/fs/fs_zip.go` (589 líneas) — `Zip` con índice inmemoria del
  directorio central (`entries` + `children` ordenados, raíz `/` implícita),
  `NewZip` (saneado §5: `\`→`/`, drive letter, `..` que escapa → omitir+warning,
  `\0` → inválido; duplicados última-gana; colisión fichero/directorio → omitir+warning;
  directorios implícitos con mtime = máximo de sus descendientes, procesando por
  profundidad decreciente), `zipFile` (lazy `fh.Open()`, streaming, CRC verificado al
  EOF), `zipDir`, `Close()` idempotente.
- Decisiones: modo Unix solo si `CreatorVersion>>8 == 3` (unix) o `== 19` (macOS,
  valor que usa la stdlib de Go 1.26) vía `fh.Mode()`; si no, 0644/0755. Symlink →
  `os.ModeSymlink|0o777`, target leído al indexar (tope 4096 B). `NewZip` normaliza
  `fh.Modified.UTC()` (el reader devuelve location `""`).
- Pendiente: nada.

### A2 — Tests unitarios ZipFS (commits `b50982c5f`, `db125ca28`)

- Qué hice: `internal/fs/fs_zip_test.go` — 14 tests `TestZip*` (jerarquía, saneado,
  duplicados, colisión, symlink, CP437, cifrado, compresión no soportada, CRC corrupto,
  Zip64, concurrencia con `-race`, no-escritura a disco, zip vacío, consistencia Lstat).
  Helpers de parcheo de binario `patchZipHeaderFlags` / `patchZipEntryMethod` /
  `patchZip64Central` (offsets locales: flags 6, method 8, nlen 26, name 30;
  central: flags 8, method 10, nlen 28, name 46, xlen 30, comment 32, offset 42).
- Decisiones: el test Zip64 marca los 3 campos de 32 bits a `0xFFFFFFFF` y mueve los
  valores reales al extra field `0x0001` — el reader de Go 1.26 exige los tres valores
  en el extra (solo perdona `uncompressed` "42.zip").
- Pendiente: nada.

### A3 — Integración CLI (commit `779cff54e`)

- Qué hice: `cmd/restic/cmd_backup.go` — campo `Migrate string` en `BackupOptions`,
  flag `--migrate`, validaciones en `Check()` (incompatible con args posicionales,
  `--stdin`, `--stdin-from-command`, `--files-from*`, `--parent`),
  `collectTargets` → `[]string{"/"}`, bloque migrate en `runBackup` (`Force=true`,
  `NewZip` con `printer.E` ANTES de `openWithAppendLock` → fail-fast sin tocar el repo,
  `defer Close()`), `targetFS = zipFS` tras el bloque VSS, `RejectByDevice` desactivado
  en modo migrate (el `DeviceID` del FS virtual es 0). CRC corrupto fluye por
  `arch.Error` → exit code 3.
- Decisiones: sin cambios en `internal/archiver` (Opción A del plan, target `/`).
- Pendiente: nada.

### A4 — Tests de integración (commit `d3003c754`)

- Qué hice: `cmd/restic/cmd_backup_migrate_integration_test.go` + helpers por
  plataforma (`cmd_backup_migrate_{unix,windows,memory_linux,memory_other}_test.go`) —
  happy path (ls == zip, restore byte a byte vs. árbol fuente, dedup con 2º backup
  comprobado por packs: solo pueden aparecer packs nuevos de tree/snapshot), opciones
  incompatibles, zip inexistente con repo sin init (prueba de fail-fast), `--exclude`,
  `--dry-run` (0 snapshots), y test de memoria con zip de 512 MiB (pico RSS acotado,
  `syscall.Getrusage` solo Linux, skip en `-short`).
- Decisiones: el fixture fija los mtimes a un instante con precisión de segundo
  (`os.Chtimes` + `UtimesNanoAt(..., AT_SYMLINK_NOFOLLOW)` para el symlink), porque el
  zip solo guarda segundos; el dedup se comprueba por IDs de packs (los snapshots son
  `TreeBlob`), no por tamaño de repo (muy pequeño en tests). `zip.Writer` no es
  thread-safe: el goroutine del test de memoria escribe y el main cierra.
- Pendiente: nada.

### A5 — Documentación y changelog (commit tras A4)

- Qué hice: sección "Backup from zip archive" en `doc/040_backup.rst` (uso, paths,
  limitaciones: sin padre/`--force`, modos según creador, precisión de segundo,
  symlinks, entradas omitidas) y `changelog/unreleased/pull-migrate-zip`.
- Decisiones: la URL del changelog apunta al fork (`Saiyine/restic`) porque no hay
  issue/PR upstream; se sustituirá por el link del PR al proponerlo. `restic backup
  --help` y `restic generate` (man + completions bash/zsh/fish) incluyen el flag
  automáticamente, sin cambios manuales.
- Pendiente: sustituir la URL del changelog cuando exista el PR.

### A6 — QA final

- `gofmt -l .`: solo `internal/backend/s3/s3_test.go`, preexistente en `master`
  (diff 0 contra `master`), no tocado.
- `go vet ./...` (los paquetes tocados) y `golangci-lint run` (v2.6.0): 0 issues
  tras corregir 6 hallazgos (commit de fixes: `Close()` en defer sin chequear error,
  `SetModTime` deprecado → campo `Modified`, helper sin usar).
- `go test -race ./internal/fs/... ./internal/archiver/... ./cmd/restic/...`: verde.
- Cross-compile: `GOOS=windows` y `GOOS=darwin` `go build ./...`: OK.
- Grep anti-extracción (`os.Create*|MkdirTemp|CreateTemp|WriteFile|ioutil.Temp`)
  sobre `fs_zip.go` y `cmd_backup.go`: nada.
- End-to-end manual con zip real (129 MiB, modos 700/644, symlink, mtimes) dentro de
  `unshare -U -r -m` con tmpfs en `/tmp`: `ls` coincide con `unzip -l`; restore
  idéntico byte a byte (modos y mtimes incluidos); en `/tmp` solo existieron repo,
  cache y target de restore → cero ficheros temporales.
- Pendiente: nada. Criterios del §7 todos verificados.

### A7 — `--migrate` repetible (varios zips)

- Qué hice: `--migrate` ahora se puede especificar varias veces
  (`Migrate []string`, flag `StringArrayVar`). `fs.NewZip` delega en el nuevo
  `fs.NewZipMulti(zipPaths, warnf)`, que abre todos los archivos, concatena sus
  entradas en orden y las indexa con la MISMA lógica que ya existía (`addEntry`):
  duplicado mismo tipo → gana el último (zip posterior) + warning; colisión
  fichero/directorio → gana la primera + warning. `Zip.rzs []*zip.ReadCloser`
  (antes `rc`); `Close()` cierra todos. CLI: validación de elementos vacíos y
  de duplicados en `Check()`.
- Decisiones: semántica de "concatenación de entradas" (no hay reglas nuevas,
  reutiliza el comportamiento de duplicados/colisiones de un solo zip); sin
  env var (igual que `--exclude-if-present`, `StringArrayVar` no la soporta).
- Tests: 4 unitarios nuevos (`TestZipMulti{Disjoint,DuplicateLastWins,TypeConflict,OpenError}`)
  e integración `TestBackupMigrateMultipleArchives` (unión + último-gana,
  restore comparado con el árbol esperado fusionado).
- Docs/changelog actualizados; pendientes: QA completo y PR.

### A8 — soporte 7z (`--migrate` zip y 7z)

- Qué hice: `--migrate` ahora acepta zips y 7z en cualquier combinación.
  Refactor del core: el tipo `Zip` se eliminó y vive ahora en
  `MigrateFS` (`internal/fs/fs_migrate.go`) con entradas neutrales por
  formato (`archiveEntry`); `fs_zip.go` solo conserva lo específico de zip
  (CP437→UTF-8, creators de modo, limpieza de nombres) y `fs_sevenzip.go`
  el indexer de 7z. `NewZip`/`NewZipMulti`/`NewSevenZip`/`NewSevenZipMulti`
  devuelven `*MigrateFS`; `NewMigrate(paths, warnf)` detecta el formato por
  magic bytes (`PK\x03\x04` / `37 7A BC AF 27 1C`), falla rápido probando
  todos los paths antes de abrir ninguno, y forma un solo snapshot con la
  misma semántica de merge que A7 (duplicado → gana el último; colisión
  fichero/directorio → gana el primero; warnings con etiqueta `zip:`/`7z:`).
- 7z: librería `github.com/bodgit/sevenzip` v1.6.5 (pure Go, BSD-3, decode
  por streaming con pool LRU de folders; codecs Copy/Delta/LZMA/LZMA2/Zstd/
  Deflate/Bzip2/LZ4/Brotli/PPMD y filtros BCJ; headers comprimidos,
  volúmenes split y SFX). El método de compresión no es público en la lib,
  así que la validación se hace por stream al indexar: se hace un peek
  (Open+1 byte+Close) de la primera entrada de cada carpeta y, si el stream
  falla (método no soportado, cifrado, datos corruptos), se omiten todas
  sus entradas con warning. Cifrado → warning "entry is encrypted".
- Symlinks 7z: si el atributo POSIX del entry es `sIFLNK`, el target se lee
  del contenido (≤4096 B). Ojo: el binario 7z local (7-Zip 26.06)
  dereferencia symlinks al crear el archivo, así que el fixture
  `symlink.7z` se genera parcheando el binario (attrs `0x81A08020`→
  `0xA1FF8020` y recompute de los dos CRC de headers; layout: start header
  de 32 bytes, NextHeaderOffset relativo al final del start header,
  NextHeaderCRC en offset 28 sobre el header, y el CRC del start header
  cubre 12–32 así que se actualiza al final).
- Testdata 7z: fixtures binarios commiteados en
  `internal/fs/testdata/sevenzip/` + `generate.py` reproducible (no depende
  de `7z` al correr los tests). El 7z local no soporta `-m0=ZSTD`, así que
  `zstd.7z` se genera con `py7zr` (fallback automático); `empty.7z` también
  usa `py7zr` (no hay switch CLI). `solid.7z` (LZMA2 + `-mhc=off`) es el
  fixture de corrupción: al voltear un byte del payload, la detección
  ocurre en el index (validación por stream) y las 3 entradas se omiten
  con warning.
- CLI: `cmd_backup.go` usa `fs.NewMigrate` (antes `NewZipMulti`); help del
  flag: "zip or 7z".
- Tests: 13 unitarios nuevos en `fs` (`TestSevenZip*`: jerarquía, codecs
  Zstd/LZMA/Copy, modos, symlink, unicode, vacío, duplicados, colisiones
  ambas direcciones, cifrado→skip, corrupto→skip con warnings,
  concurrencia -race, no-acceso-a-disco, consistencia Lstat) y 4 en
  `TestMigrate*` (mezcla zip+7z disjunta, duplicado cross-format, colisión
  cross-format, formatos no soportados/fichero corto/faltante/vacío).
  Integración: `TestBackupMigrateSevenZip` y
  `TestBackupMigrateMixedZipSevenZip` (skip si no hay `7z` en PATH;
  fixtures con mtimes enteros vía `Chtimes`).
- go.mod: `sevenzip` pasa a dependencia directa; nuevas indirectas
  (`bodgit/plumbing`, `bodgit/windows`, `andybalholm/brotli`,
  `stangelandcl/ppmd`, `spf13/afero`, `go4.org`) — footprint relevante para
  upstream.
- Docs/changelog: sección de `040_backup.rst` ampliada a "Backup from zip
  or 7z archives" (nota sobre symlinks/7z y métodos soportados); changelog
  renombrado a `pull-migrate-zip-and-7z`.
