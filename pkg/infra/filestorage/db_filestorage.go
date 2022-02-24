package filestorage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/sqlstore"
)

type file struct {
	Path             string    `xorm:"path"`
	ParentFolderPath string    `xorm:"parent_folder_path"`
	Contents         []byte    `xorm:"contents"`
	Updated          time.Time `xorm:"updated"`
	Created          time.Time `xorm:"created"`
	Size             int64     `xorm:"size"`
	MimeType         string    `xorm:"mime_type"`
}

type fileMeta struct {
	Path    string    `xorm:"path"`
	Key     string    `xorm:"key"`
	Value   string    `xorm:"value"`
	Updated time.Time `xorm:"updated"`
	Created time.Time `xorm:"created"`
}

type dbFileStorage struct {
	db  *sqlstore.SQLStore
	log log.Logger
}

func NewDbStorage(log log.Logger, db *sqlstore.SQLStore, pathFilters *PathFilters) FileStorage {
	return &wrapper{
		log: log,
		wrapped: &dbFileStorage{
			log: log,
			db:  db,
		},
		pathFilters: pathFilters,
	}
}

func (s dbFileStorage) getProperties(sess *sqlstore.DBSession, lowerCasePaths []string) (map[string]map[string]string, error) {
	attributesByPath := make(map[string]map[string]string, 0)

	entities := make([]*fileMeta, 0)
	if err := sess.Table("file_meta").In("path", lowerCasePaths).Find(&entities); err != nil {
		return nil, err
	}

	for _, entity := range entities {
		if _, ok := attributesByPath[entity.Path]; !ok {
			attributesByPath[entity.Path] = make(map[string]string)
		}
		attributesByPath[entity.Path][entity.Key] = entity.Value
	}

	return attributesByPath, nil
}

func (s dbFileStorage) Get(ctx context.Context, filePath string) (*File, error) {
	var result *File
	err := s.db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		table := &file{}
		exists, err := sess.Table("file").Where("LOWER(path) = ?", strings.ToLower(filePath)).Get(table)
		if !exists {
			return nil
		}

		var meta = make([]*fileMeta, 0)
		if err := sess.Table("file_meta").Where("path = ?", strings.ToLower(filePath)).Find(&meta); err != nil {
			return err
		}

		var metaProperties = make(map[string]string, len(meta))

		for i := range meta {
			metaProperties[meta[i].Key] = meta[i].Value
		}

		result = &File{
			Contents: table.Contents,
			FileMetadata: FileMetadata{
				Name:       getName(table.Path),
				FullPath:   table.Path,
				Created:    table.Created,
				Properties: metaProperties,
				Modified:   table.Updated,
				Size:       table.Size,
				MimeType:   table.MimeType,
			},
		}
		return err
	})

	return result, err
}

func (s dbFileStorage) Delete(ctx context.Context, filePath string) error {
	err := s.db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		table := &file{}
		exists, err := sess.Table("file").Where("LOWER(path) = ?", strings.ToLower(filePath)).Get(table)
		if !exists {
			return nil
		}

		number, err := sess.Table("file").Where("LOWER(path) = ?", strings.ToLower(filePath)).Delete(table)
		if err != nil {
			return err
		}
		s.log.Info("Deleted file", "path", filePath, "affectedRecords", number)

		metaTable := &fileMeta{}
		number, err = sess.Table("file_meta").Where("path = ?", strings.ToLower(filePath)).Delete(metaTable)
		if err != nil {
			return err
		}
		s.log.Info("Deleted metadata", "path", filePath, "affectedRecords", number)
		return err
	})

	return err
}

func (s dbFileStorage) Upsert(ctx context.Context, cmd *UpsertFileCommand) error {
	now := time.Now()
	err := s.db.WithTransactionalDbSession(ctx, func(sess *sqlstore.DBSession) error {
		existing := &file{}
		exists, err := sess.Table("file").Where("LOWER(path) = ?", strings.ToLower(cmd.Path)).Get(existing)
		if err != nil {
			return err
		}

		if exists {
			existing.Updated = now
			if cmd.Contents != nil {
				contents := *cmd.Contents
				existing.Contents = contents
				existing.MimeType = cmd.MimeType
				existing.Size = int64(len(contents))
			}

			_, err = sess.Where("LOWER(path) = ?", strings.ToLower(cmd.Path)).Update(existing)
			if err != nil {
				return err
			}
		} else {
			contentsToInsert := make([]byte, 0)
			if cmd.Contents != nil {
				contentsToInsert = *cmd.Contents
			}

			file := &file{
				Path:             cmd.Path,
				ParentFolderPath: getParentFolderPath(cmd.Path),
				Contents:         contentsToInsert,
				MimeType:         cmd.MimeType,
				Size:             int64(len(contentsToInsert)),
				Updated:          now,
				Created:          now,
			}
			_, err := sess.Insert(file)
			if err != nil {
				return err
			}
		}

		err = upsertProperties(sess, now, cmd)
		if err != nil {
			if rollbackErr := sess.Rollback(); rollbackErr != nil {
				s.log.Error("failed while rolling back upsert", "path", cmd.Path)
			}
			return err
		}
		return err
	})

	return err
}

func upsertProperties(sess *sqlstore.DBSession, now time.Time, cmd *UpsertFileCommand) error {
	if len(cmd.Properties) == 0 {
		return nil
	}

	for key, val := range cmd.Properties {
		if err := upsertProperty(sess, now, cmd.Path, key, val); err != nil {
			return err
		}
	}
	return nil
}

func upsertProperty(sess *sqlstore.DBSession, now time.Time, path string, key string, val string) error {
	existing := &fileMeta{}
	exists, err := sess.Table("file_meta").Where("path = ? AND key = ?", strings.ToLower(path), key).Get(existing)
	if err != nil {
		return err
	}

	if exists {
		existing.Updated = now
		existing.Value = val
		_, err = sess.Where("path = ? AND key = ?", strings.ToLower(path), key).Update(existing)
	} else {
		_, err = sess.Insert(&fileMeta{
			Path:    strings.ToLower(path),
			Key:     key,
			Value:   val,
			Updated: now,
			Created: now,
		})
	}
	return err
}

func (s dbFileStorage) ListFiles(ctx context.Context, folderPath string, paging *Paging, options *ListOptions) (*ListFilesResponse, error) {
	var resp *ListFilesResponse

	err := s.db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		var foundFiles = make([]*file, 0)

		sess.Table("file")
		if options.Recursive {
			sess.Where("LOWER(parent_folder_path) LIKE ?", fmt.Sprintf("%s%s", strings.ToLower(folderPath), "%"))
		} else {
			sess.Where("LOWER(parent_folder_path) = ?", strings.ToLower(folderPath))
		}
		sess.Where("LOWER(path) NOT LIKE ?", fmt.Sprintf("%s%s%s", "%", Delimiter, directoryMarker))

		for _, prefix := range options.PathFilters.allowedPrefixes {
			sess.Where("LOWER(path) LIKE ?", fmt.Sprintf("%s%s", strings.ToLower(prefix), "%"))
		}

		sess.OrderBy("path")

		pageSize := paging.First
		sess.Limit(pageSize + 1)

		if paging != nil && paging.After != "" {
			sess.Where("path > ?", paging.After)
		}

		if err := sess.Find(&foundFiles); err != nil {
			return err
		}

		foundLength := len(foundFiles)
		if foundLength > pageSize {
			foundLength = pageSize
		}

		lowerCasePaths := make([]string, 0)
		for i := 0; i < foundLength; i++ {
			lowerCasePaths = append(lowerCasePaths, strings.ToLower(foundFiles[i].Path))
		}
		propertiesByLowerPath, err := s.getProperties(sess, lowerCasePaths)
		if err != nil {
			return err
		}

		files := make([]FileMetadata, 0)
		for i := 0; i < foundLength; i++ {
			var props map[string]string
			path := foundFiles[i].Path
			if foundProps, ok := propertiesByLowerPath[strings.ToLower(path)]; ok {
				props = foundProps
			} else {
				props = make(map[string]string, 0)
			}

			files = append(files, FileMetadata{
				Name:       getName(path),
				FullPath:   path,
				Created:    foundFiles[i].Created,
				Properties: props,
				Modified:   foundFiles[i].Updated,
				Size:       foundFiles[i].Size,
				MimeType:   foundFiles[i].MimeType,
			})
		}

		lastPath := ""
		if len(files) > 0 {
			lastPath = files[len(files)-1].FullPath
		}

		resp = &ListFilesResponse{
			Files:    files,
			LastPath: lastPath,
			HasMore:  len(foundFiles) == pageSize+1,
		}
		return nil
	})

	return resp, err
}

func (s dbFileStorage) ListFolders(ctx context.Context, parentFolderPath string, options *ListOptions) ([]FileMetadata, error) {
	folders := make([]FileMetadata, 0)
	err := s.db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		var foundPaths []string

		sess.Table("file")
		sess.Distinct("parent_folder_path")

		if options.Recursive {
			sess.Where("LOWER(parent_folder_path) > ?", strings.ToLower(parentFolderPath))
		} else {
			sess.Where("LOWER(parent_folder_path) = ?", strings.ToLower(parentFolderPath))
		}

		for _, prefix := range options.PathFilters.allowedPrefixes {
			sess.Where("LOWER(parent_folder_path) LIKE ?", fmt.Sprintf("%s%s", strings.ToLower(prefix), "%"))
		}

		sess.OrderBy("parent_folder_path")
		sess.Cols("parent_folder_path")

		if err := sess.Find(&foundPaths); err != nil {
			return err
		}

		mem := make(map[string]bool)
		for i := 0; i < len(foundPaths); i++ {
			path := foundPaths[i]
			parts := strings.Split(path, Delimiter)
			acc := parts[0]
			j := 1
			for {
				acc = fmt.Sprintf("%s%s%s", acc, Delimiter, parts[j])
				if !mem[acc] && len(acc) > len(parentFolderPath) {
					folders = append(folders, FileMetadata{
						Name:     getName(acc),
						FullPath: acc,
					})
				}
				mem[acc] = true

				j += 1
				if j >= len(parts) {
					break
				}
			}
		}

		return nil
	})

	return folders, err
}

func (s dbFileStorage) CreateFolder(ctx context.Context, parentFolderPath string, folderName string) error {
	now := time.Now()
	err := s.db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		existing := &file{}

		directoryMarkerParentPath := fmt.Sprintf("%s%s%s", parentFolderPath, Delimiter, folderName)
		directoryMarkerPath := fmt.Sprintf("%s%s%s", directoryMarkerParentPath, Delimiter, directoryMarker)
		exists, err := sess.Table("file").Where("LOWER(path) = ?", strings.ToLower(directoryMarkerPath)).Get(existing)
		if err != nil {
			return err
		}

		if exists {
			return nil
		}

		file := &file{
			Path:             directoryMarkerPath,
			ParentFolderPath: directoryMarkerParentPath,
			Contents:         make([]byte, 0),
			Updated:          now,
			Created:          now,
		}
		_, err = sess.Insert(file)
		return err
	})

	return err
}

func (s dbFileStorage) DeleteFolder(ctx context.Context, folderPath string) error {
	err := s.db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		existing := &file{}
		directoryMarkerPath := fmt.Sprintf("%s%s%s", folderPath, Delimiter, directoryMarker)
		exists, err := sess.Table("file").Where("LOWER(path) = ?", strings.ToLower(directoryMarkerPath)).Get(existing)
		if err != nil {
			return err
		}

		if !exists {
			return nil
		}

		_, err = sess.Table("file").Where("LOWER(path) = ?", strings.ToLower(directoryMarkerPath)).Delete(existing)
		return err
	})

	return err
}

func (s dbFileStorage) close() error {
	return nil
}
