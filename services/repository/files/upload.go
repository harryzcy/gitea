// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package files

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	git_model "code.gitea.io/gitea/models/git"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/git/attribute"
	"code.gitea.io/gitea/modules/lfs"
	"code.gitea.io/gitea/modules/setting"
)

// UploadRepoFileOptions contains the uploaded repository file options
type UploadRepoFileOptions struct {
	LastCommitID string
	OldBranch    string
	NewBranch    string
	TreePath     string
	Message      string
	Files        []string // In UUID format.
	Signoff      bool
	Author       *IdentityOptions
	Committer    *IdentityOptions
}

type uploadInfo struct {
	upload        *repo_model.Upload
	lfsMetaObject *git_model.LFSMetaObject
}

func cleanUpAfterFailure(ctx context.Context, infos *[]uploadInfo, t *TemporaryUploadRepository, original error) error {
	for _, info := range *infos {
		if info.lfsMetaObject == nil {
			continue
		}
		if !info.lfsMetaObject.Existing {
			if _, err := git_model.RemoveLFSMetaObjectByOid(ctx, t.repo.ID, info.lfsMetaObject.Oid); err != nil {
				original = fmt.Errorf("%w, %v", original, err) // We wrap the original error - as this is the underlying error that required the fallback
			}
		}
	}
	return original
}

// UploadRepoFiles uploads files to the given repository
func UploadRepoFiles(ctx context.Context, repo *repo_model.Repository, doer *user_model.User, opts *UploadRepoFileOptions) error {
	if len(opts.Files) == 0 {
		return nil
	}

	uploads, err := repo_model.GetUploadsByUUIDs(ctx, opts.Files)
	if err != nil {
		return fmt.Errorf("GetUploadsByUUIDs [uuids: %v]: %w", opts.Files, err)
	}

	names := make([]string, len(uploads))
	infos := make([]uploadInfo, len(uploads))
	for i, upload := range uploads {
		// Check file is not lfs locked, will return nil if lock setting not enabled
		filepath := path.Join(opts.TreePath, upload.Name)
		lfsLock, err := git_model.GetTreePathLock(ctx, repo.ID, filepath)
		if err != nil {
			return err
		}
		if lfsLock != nil && lfsLock.OwnerID != doer.ID {
			u, err := user_model.GetUserByID(ctx, lfsLock.OwnerID)
			if err != nil {
				return err
			}
			return git_model.ErrLFSFileLocked{RepoID: repo.ID, Path: filepath, UserName: u.Name}
		}

		names[i] = upload.Name
		infos[i] = uploadInfo{upload: upload}
	}

	t, err := NewTemporaryUploadRepository(repo)
	if err != nil {
		return err
	}
	defer t.Close()

	hasOldBranch := true
	if err = t.Clone(ctx, opts.OldBranch, true); err != nil {
		if !git.IsErrBranchNotExist(err) || !repo.IsEmpty {
			return err
		}
		if err = t.Init(ctx, repo.ObjectFormatName); err != nil {
			return err
		}
		hasOldBranch = false
		opts.LastCommitID = ""
	}
	if hasOldBranch {
		if err = t.SetDefaultIndex(ctx); err != nil {
			return err
		}
	}

	var attributesMap map[string]*attribute.Attributes
	// when uploading to an empty repo, the old branch doesn't exist, but some "global gitattributes" or "info/attributes" may exist
	if setting.LFS.StartServer {
		attributesMap, err = attribute.CheckAttributes(ctx, t.gitRepo, "" /* use temp repo's working dir */, attribute.CheckAttributeOpts{
			Attributes: []string{attribute.Filter},
			Filenames:  names,
		})
		if err != nil {
			return err
		}
	}

	// Copy uploaded files into repository.
	// TODO: there is a small problem: when uploading LFS files with ".gitattributes", the "check-attr" runs before this loop,
	// so LFS files are not able to be added as LFS objects. Ideally we need to do in 3 steps in the future:
	// 1. Add ".gitattributes" to git index
	// 2. Run "check-attr" (the previous attribute.CheckAttributes call)
	// 3. Add files to git index (this loop)
	// This problem is trivial so maybe no need to spend too much time on it at the moment.
	for i := range infos {
		if err := copyUploadedLFSFileIntoRepository(ctx, &infos[i], attributesMap, t, opts.TreePath); err != nil {
			return err
		}
	}

	// Now write the tree
	treeHash, err := t.WriteTree(ctx)
	if err != nil {
		return err
	}

	// Now commit the tree
	commitOpts := &CommitTreeUserOptions{
		ParentCommitID:    opts.LastCommitID,
		TreeHash:          treeHash,
		CommitMessage:     opts.Message,
		SignOff:           opts.Signoff,
		DoerUser:          doer,
		AuthorIdentity:    opts.Author,
		CommitterIdentity: opts.Committer,
	}
	commitHash, err := t.CommitTree(ctx, commitOpts)
	if err != nil {
		return err
	}

	// Now deal with LFS objects
	for i := range infos {
		if infos[i].lfsMetaObject == nil {
			continue
		}
		infos[i].lfsMetaObject, err = git_model.NewLFSMetaObject(ctx, infos[i].lfsMetaObject.RepositoryID, infos[i].lfsMetaObject.Pointer)
		if err != nil {
			// OK Now we need to cleanup
			return cleanUpAfterFailure(ctx, &infos, t, err)
		}
		// Don't move the files yet - we need to ensure that
		// everything can be inserted first
	}

	// OK now we can insert the data into the store - there's no way to clean up the store
	// once it's in there, it's in there.
	contentStore := lfs.NewContentStore()
	for _, info := range infos {
		if err := uploadToLFSContentStore(info, contentStore); err != nil {
			return cleanUpAfterFailure(ctx, &infos, t, err)
		}
	}

	// Then push this tree to NewBranch
	if err := t.Push(ctx, doer, commitHash, opts.NewBranch); err != nil {
		return err
	}

	return repo_model.DeleteUploads(ctx, uploads...)
}

func copyUploadedLFSFileIntoRepository(ctx context.Context, info *uploadInfo, attributesMap map[string]*attribute.Attributes, t *TemporaryUploadRepository, treePath string) error {
	file, err := os.Open(info.upload.LocalPath())
	if err != nil {
		return err
	}
	defer file.Close()

	var objectHash string
	if setting.LFS.StartServer && attributesMap[info.upload.Name] != nil && attributesMap[info.upload.Name].Get(attribute.Filter).ToString().Value() == "lfs" {
		// Handle LFS
		// FIXME: Inefficient! this should probably happen in models.Upload
		pointer, err := lfs.GeneratePointer(file)
		if err != nil {
			return err
		}

		info.lfsMetaObject = &git_model.LFSMetaObject{Pointer: pointer, RepositoryID: t.repo.ID}

		if objectHash, err = t.HashObjectAndWrite(ctx, strings.NewReader(pointer.StringContent())); err != nil {
			return err
		}
	} else if objectHash, err = t.HashObjectAndWrite(ctx, file); err != nil {
		return err
	}

	// Add the object to the index
	return t.AddObjectToIndex(ctx, "100644", objectHash, path.Join(treePath, info.upload.Name))
}

func uploadToLFSContentStore(info uploadInfo, contentStore *lfs.ContentStore) error {
	if info.lfsMetaObject == nil {
		return nil
	}
	exist, err := contentStore.Exists(info.lfsMetaObject.Pointer)
	if err != nil {
		return err
	}
	if !exist {
		file, err := os.Open(info.upload.LocalPath())
		if err != nil {
			return err
		}

		defer file.Close()
		// FIXME: Put regenerates the hash and copies the file over.
		// I guess this strictly ensures the soundness of the store but this is inefficient.
		if err := contentStore.Put(info.lfsMetaObject.Pointer, file); err != nil {
			// OK Now we need to cleanup
			// Can't clean up the store, once uploaded there they're there.
			return err
		}
	}
	return nil
}
