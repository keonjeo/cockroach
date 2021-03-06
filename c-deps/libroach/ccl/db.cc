// Copyright 2017 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

#include "../db.h"
#include <iostream>
#include <libroachccl.h>
#include <memory>
#include <rocksdb/comparator.h>
#include <rocksdb/iterator.h>
#include <rocksdb/utilities/write_batch_with_index.h>
#include <rocksdb/write_batch.h>
#include "../batch.h"
#include "../comparator.h"
#include "../encoding.h"
#include "../env_manager.h"
#include "../rocksdbutils/env_encryption.h"
#include "../status.h"
#include "ccl/baseccl/encryption_options.pb.h"
#include "ccl/storageccl/engineccl/enginepbccl/stats.pb.h"
#include "ctr_stream.h"
#include "key_manager.h"

using namespace cockroach;

namespace cockroach {

class CCLEnvStatsHandler : public EnvStatsHandler {
 public:
  explicit CCLEnvStatsHandler(KeyManager* store_key_manager, KeyManager* data_key_manager)
      : store_key_manager_(store_key_manager), data_key_manager_(data_key_manager) {}
  virtual ~CCLEnvStatsHandler() {}

  virtual rocksdb::Status GetEncryptionStats(std::string* serialized_stats) override {
    enginepbccl::EncryptionStatus enc_status;

    bool has_stats = false;
    if (this->store_key_manager_ != nullptr) {
      has_stats = true;
      // Transfer ownership of new key info to status proto, this frees any previous value.
      enc_status.set_allocated_active_store_key(store_key_manager_->CurrentKeyInfo().release());
    }

    if (this->data_key_manager_ != nullptr) {
      has_stats = true;
      // Transfer ownership of new key info to status proto, this frees any previous value.
      enc_status.set_allocated_active_data_key(data_key_manager_->CurrentKeyInfo().release());
    }

    if (!has_stats) {
      return rocksdb::Status::OK();
    }

    if (!enc_status.SerializeToString(serialized_stats)) {
      return rocksdb::Status::InvalidArgument("failed to serialize encryption status");
    }
    return rocksdb::Status::OK();
  }

 private:
  // KeyManagers are needed to get key information but are not owned by the StatsHandler.
  KeyManager* store_key_manager_;
  KeyManager* data_key_manager_;
};

// DBOpenHook parses the extra_options field of DBOptions and initializes encryption objects if
// needed.
rocksdb::Status DBOpenHook(const std::string& db_dir, const DBOptions db_opts,
                           EnvManager* env_mgr) {
  DBSlice options = db_opts.extra_options;
  if (options.len == 0) {
    return rocksdb::Status::OK();
  }

  // The Go code sets the "file_registry" storage version if we specified encryption flags,
  // but let's double check anyway.
  if (!db_opts.use_file_registry) {
    return rocksdb::Status::InvalidArgument(
        "on-disk version does not support encryption, but we found encryption flags");
  }

  // Parse extra_options.
  cockroach::ccl::baseccl::EncryptionOptions opts;
  if (!opts.ParseFromArray(options.data, options.len)) {
    return rocksdb::Status::InvalidArgument("failed to parse extra options");
  }

  if (opts.key_source() != cockroach::ccl::baseccl::KeyFiles) {
    return rocksdb::Status::InvalidArgument("unknown encryption key source");
  }

  // Initialize store key manager.
  // NOTE: FileKeyManager uses the default env as the MemEnv can never have pre-populated files.
  FileKeyManager* store_key_manager = new FileKeyManager(
      rocksdb::Env::Default(), opts.key_files().current_key(), opts.key_files().old_key());
  rocksdb::Status status = store_key_manager->LoadKeys();
  if (!status.ok()) {
    delete store_key_manager;
    return status;
  }

  // Create a cipher stream creator using the store_key_manager.
  auto store_stream = new CTRCipherStreamCreator(store_key_manager, enginepb::Store);

  // Construct an EncryptedEnv using this stream creator and the base_env (Default or Mem).
  // It takes ownership of the stream.
  rocksdb::Env* store_keyed_env =
      rocksdb_utils::NewEncryptedEnv(env_mgr->base_env, env_mgr->file_registry.get(), store_stream);
  // Transfer ownership to the env manager.
  env_mgr->TakeEnvOwnership(store_keyed_env);

  // Initialize data key manager using the stored-keyed-env.
  DataKeyManager* data_key_manager =
      new DataKeyManager(store_keyed_env, db_dir, opts.data_key_rotation_period());
  status = data_key_manager->LoadKeys();
  if (!status.ok()) {
    delete data_key_manager;
    return status;
  }

  // Create a cipher stream creator using the data_key_manager.
  auto data_stream = new CTRCipherStreamCreator(data_key_manager, enginepb::Data);

  // Construct an EncryptedEnv using this stream creator and the base_env (Default or Mem).
  // It takes ownership of the stream.
  rocksdb::Env* data_keyed_env =
      rocksdb_utils::NewEncryptedEnv(env_mgr->base_env, env_mgr->file_registry.get(), data_stream);
  // Transfer ownership to the env manager and make it as the db_env.
  env_mgr->TakeEnvOwnership(data_keyed_env);
  env_mgr->db_env = data_keyed_env;

  // Fetch the current store key info.
  std::unique_ptr<enginepbccl::KeyInfo> store_key = store_key_manager->CurrentKeyInfo();
  assert(store_key != nullptr);

  // Generate a new data key if needed by giving the active store key info to the data key
  // manager.
  status = data_key_manager->SetActiveStoreKey(std::move(store_key));
  if (!status.ok()) {
    return status;
  }

  // Everything's ok: initialize a stats handler.
  env_mgr->SetStatsHandler(new CCLEnvStatsHandler(store_key_manager, data_key_manager));

  return rocksdb::Status::OK();
}

}  // namespace cockroach

DBStatus DBBatchReprVerify(DBSlice repr, DBKey start, DBKey end, int64_t now_nanos,
                           MVCCStatsResult* stats) {
  // TODO(dan): Inserting into a batch just to iterate over it is unfortunate.
  // Consider replacing this with WriteBatch's Iterate/Handler mechanism and
  // computing MVCC stats on the post-ApplyBatchRepr engine. splitTrigger does
  // the latter and it's a headache for propEvalKV, so wait to see how that
  // settles out before doing it that way.
  rocksdb::WriteBatchWithIndex batch(&kComparator, 0, true);
  rocksdb::WriteBatch b(ToString(repr));
  std::unique_ptr<rocksdb::WriteBatch::Handler> inserter(GetDBBatchInserter(&batch));
  rocksdb::Status status = b.Iterate(inserter.get());
  if (!status.ok()) {
    return ToDBStatus(status);
  }
  std::unique_ptr<rocksdb::Iterator> iter;
  iter.reset(batch.NewIteratorWithBase(rocksdb::NewEmptyIterator()));

  iter->SeekToFirst();
  if (iter->Valid() && kComparator.Compare(iter->key(), EncodeKey(start)) < 0) {
    return FmtStatus("key not in request range");
  }
  iter->SeekToLast();
  if (iter->Valid() && kComparator.Compare(iter->key(), EncodeKey(end)) >= 0) {
    return FmtStatus("key not in request range");
  }

  *stats = MVCCComputeStatsInternal(iter.get(), start, end, now_nanos);

  return kSuccess;
}
