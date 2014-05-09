package wallet

import (
	"io"
	"os"
	"fmt"
	"sort"
	"sync"
	"bytes"
	"io/ioutil"
	"encoding/binary"
	"github.com/piotrnar/gocoin/btc"
	"github.com/piotrnar/gocoin/qdb"
	"github.com/piotrnar/gocoin/client/common"
)

var (
	BalanceMutex sync.Mutex
	MyBalance btc.AllUnspentTx  // unspent outputs that can be removed
	MyWallet *OneWallet     // addresses that cann be poped up
	LastBalance uint64
	BalanceChanged bool
	BalanceInvalid bool = true

	CachedAddrs map[[20]byte] *OneCachedAddrBalance = make(map[[20]byte] *OneCachedAddrBalance)
	CacheUnspent [] *OneCachedUnspent
	CacheUnspentIdx map[uint64] *OneCachedUnspentIdx = make(map[uint64] *OneCachedUnspentIdx)
)


type OneCachedUnspentIdx struct {
	Index uint
	Record *btc.OneUnspentTx
}


type OneCachedUnspent struct {
	*btc.BtcAddr
	btc.AllUnspentTx  // a cache for unspent outputs (from different wallets)
}

type OneCachedAddrBalance struct {
	InWallet bool
	CacheIndex uint
	Value uint64
}


func po2idx(po *btc.TxPrevOut) uint64 {
	return binary.LittleEndian.Uint64(po.Hash[:8]) ^ uint64(po.Vout)
}


// This is called while accepting the block (from the chain's thread)
func TxNotify (idx *btc.TxPrevOut, valpk *btc.TxOut) {
	var update_wallet bool

	BalanceMutex.Lock()
	defer BalanceMutex.Unlock()

	if valpk!=nil {
		// Extract hash160 from pkscript
		adr := btc.NewAddrFromPkScript(valpk.Pk_script, common.Testnet)
		if adr==nil {
			return // We do not monitor this address
		}

		if rec, ok := CachedAddrs[adr.Hash160]; ok {
			rec.Value += valpk.Value
			utxo := new(btc.OneUnspentTx)
			utxo.TxPrevOut = *idx
			utxo.Value = valpk.Value
			utxo.MinedAt = valpk.BlockHeight
			utxo.BtcAddr = CacheUnspent[rec.CacheIndex].BtcAddr
			CacheUnspent[rec.CacheIndex].AllUnspentTx = append(CacheUnspent[rec.CacheIndex].AllUnspentTx, utxo)
			CacheUnspentIdx[po2idx(idx)] = &OneCachedUnspentIdx{Index: rec.CacheIndex, Record: utxo}
			if rec.InWallet {
				update_wallet = true
			}
		}
	} else {
		ii := po2idx(idx)
		if ab, present := CacheUnspentIdx[ii]; present {
			adrec := CacheUnspent[ab.Index]
			//println("removing", idx.String())
			rec := CachedAddrs[adrec.BtcAddr.Hash160]
			if rec==nil {
				panic("rec not found for " + adrec.BtcAddr.String())
			}
			rec.Value -= ab.Record.Value
			if rec.InWallet {
				update_wallet = true
			}
			for j := range adrec.AllUnspentTx {
				if adrec.AllUnspentTx[j] == ab.Record {
					//println("found it at index", j)
					adrec.AllUnspentTx = append(adrec.AllUnspentTx[:j], adrec.AllUnspentTx[j+1:]...)
					break
				}
			}
			delete(CacheUnspentIdx, ii)
		}
	}

	if update_wallet {
		SyncWallet()
	}
}


func SyncWallet() {
	if MyWallet!=nil {
		MyBalance = nil
		for i := range MyWallet.Addrs {
			var rec *OneCachedAddrBalance
			if MyWallet.Addrs[i].StealthAddr!=nil {
				var h160 [20]byte
				copy(h160[:], MyWallet.Addrs[i].StealthAddr.Hash160())
				rec = CachedAddrs[h160]
			} else {
				rec = CachedAddrs[MyWallet.Addrs[i].Hash160]
			}
			if rec==nil {
				println("his should not happen: No record in the cache", MyWallet.Addrs[i].String())
			}
			MyBalance = append(MyBalance, CacheUnspent[rec.CacheIndex].AllUnspentTx...)
		}
		sort_and_sum()
		BalanceChanged = true
	}
}


func GetRawTransaction(BlockHeight uint32, txid *btc.Uint256, txf io.Writer) bool {
	// Find the block with the indicated Height in the main tree
	common.BlockChain.BlockIndexAccess.Lock()
	n := common.Last.Block
	if n.Height < BlockHeight {
		println(n.Height, BlockHeight)
		common.BlockChain.BlockIndexAccess.Unlock()
		panic("This should not happen")
	}
	for n.Height > BlockHeight {
		n = n.Parent
	}
	common.BlockChain.BlockIndexAccess.Unlock()

	bd, _, e := common.BlockChain.Blocks.BlockGet(n.BlockHash)
	if e != nil {
		println("BlockGet", n.BlockHash.String(), BlockHeight, e.Error())
		println("This should not happen - please, report a bug.")
		println("You can probably fix it by launching the client with -rescan")
		os.Exit(1)
	}

	bl, e := btc.NewBlock(bd)
	if e != nil {
		println("NewBlock: ", e.Error())
		os.Exit(1)
	}

	e = bl.BuildTxList()
	if e != nil {
		println("BuildTxList:", e.Error())
		os.Exit(1)
	}

	// Find the transaction we need and store it in the file
	for i := range bl.Txs {
		if bl.Txs[i].Hash.Equal(txid) {
			txf.Write(bl.Txs[i].Serialize())
			return true
		}
	}
	return false
}


// Call it only from the Chain thread
func DumpBalance(mybal btc.AllUnspentTx, utxt *os.File, details, update_balance bool) (s string) {
	var sum uint64
	BalanceMutex.Lock()
	defer BalanceMutex.Unlock()

	for i := range mybal {
		sum += mybal[i].Value

		if details {
			if i<100 {
				s += fmt.Sprintf("%7d %s\n", 1+common.Last.Block.Height-mybal[i].MinedAt,
					mybal[i].String())
			} else if i==100 {
				s += fmt.Sprintln("List of unspent outputs truncated to 100 records")
			}
		}

		// update the balance/ folder
		if utxt != nil {
			po, e := common.BlockChain.Unspent.UnspentGet(&mybal[i].TxPrevOut)
			if e != nil {
				println("UnspentGet:", e.Error())
				println("This should not happen - please, report a bug.")
				println("You can probably fix it by launching the client with -rescan")
				os.Exit(1)
			}

			txid := btc.NewUint256(mybal[i].TxPrevOut.Hash[:])

			// Store the unspent line in balance/unspent.txt
			fmt.Fprintln(utxt, mybal[i].UnspentTextLine())

			// store the entire transactiojn in balance/<txid>.tx
			fn := "balance/"+txid.String()[:64]+".tx"
			txf, _ := os.Open(fn)
			if txf == nil {
				// Do it only once per txid
				txf, _ = os.Create(fn)
				if txf==nil {
					println("Cannot create ", fn)
					os.Exit(1)
				}
				GetRawTransaction(po.BlockHeight, txid, txf)
			}
			txf.Close()
		}
	}
	if update_balance {
		LastBalance = sum
	}
	s += fmt.Sprintf("Total balance: %.8f BTC in %d unspent outputs\n", float64(sum)/1e8, len(mybal))
	if utxt != nil {
		utxt.Close()
	}
	return
}


func UpdateBalance() {
	var tofetch_stealh []*btc.BtcAddr
	var tofetch_secrets [][]byte
	var skip_stealths bool
	tofetch_regular := make(map[uint64]*btc.BtcAddr)

	BalanceMutex.Lock()
	defer BalanceMutex.Unlock()

	MyBalance = nil

	for _, v := range CachedAddrs {
		v.InWallet = false
	}

	FetchStealthKeys()

	for i := range MyWallet.Addrs {
		if rec, pres := CachedAddrs[MyWallet.Addrs[i].Hash160]; pres {
			rec.InWallet = true
			for j := range CacheUnspent[rec.CacheIndex].AllUnspentTx {
				// update BtcAddr in each of AllUnspentTx to reflect the latest label
				CacheUnspent[rec.CacheIndex].AllUnspentTx[j].BtcAddr = MyWallet.Addrs[i]
			}
			MyBalance = append(MyBalance, CacheUnspent[rec.CacheIndex].AllUnspentTx...)
		} else {
			add_it := true
			// Add a new address to the balance cache
			if MyWallet.Addrs[i].StealthAddr==nil {
				tofetch_regular[MyWallet.Addrs[i].AIdx()] = MyWallet.Addrs[i]
			} else if !skip_stealths {
				sa := MyWallet.Addrs[i].StealthAddr
				for j:=0; ; { // check if we have a matching stealth secret
					if bytes.Equal(btc.PublicFromPrivate(StealthSecrets[j], true), sa.ScanKey[:]) {
						tofetch_stealh = append(tofetch_stealh, MyWallet.Addrs[i])
						tofetch_secrets = append(tofetch_secrets, StealthSecrets[j])
						var rec stealthCacheRec
						rec.addr = MyWallet.Addrs[i]
						copy(rec.d[:], StealthSecrets[j])
						copy(rec.h160[:], MyWallet.Addrs[i].Hash160[:])
						StealthAdCache = append(StealthAdCache, rec)
						break
					} else if j==len(StealthSecrets)-1 {
						if !PrecachingComplete {
							fmt.Println("No matching secret for", sa.String())
						}
						add_it = false
						break
					}
					j++
				}
			}
			if add_it {
				CachedAddrs[MyWallet.Addrs[i].Hash160] = &OneCachedAddrBalance{InWallet:true, CacheIndex:uint(len(CacheUnspent))}
				CacheUnspent = append(CacheUnspent, &OneCachedUnspent{BtcAddr:MyWallet.Addrs[i]})
			}
		}
	}

	if len(tofetch_regular)>0 || len(tofetch_stealh)>0 {
		fmt.Println("Fetching a new blance for", len(tofetch_regular), "regular and", len(tofetch_stealh), "stealth addresses")
		// There are new addresses which we have not monitored yet
		var new_addrs btc.AllUnspentTx

		common.BlockChain.Unspent.BrowseUTXO(false, func(db *qdb.DB, k qdb.KeyType, rec *btc.OneWalkRecord) (uint32) {
			if rec.IsP2KH() {
				if ad, ok := tofetch_regular[binary.LittleEndian.Uint64(rec.Script()[3:3+8])]; ok {
					new_addrs = append(new_addrs, rec.ToUnspent(ad))
				}
			} else if rec.IsP2SH() {
				if ad, ok := tofetch_regular[binary.LittleEndian.Uint64(rec.Script()[2:2+8])]; ok {
					new_addrs = append(new_addrs, rec.ToUnspent(ad))
				}
			} else if rec.IsStealthIdx() {
				for i := range tofetch_stealh {
					fl, uo := CheckStealthRec(db, k, rec, tofetch_stealh[i], tofetch_secrets[i], true)
					if fl != 0 {
						return fl
					}
					if uo!=nil {
						new_addrs = append(new_addrs, uo)
						break
					}
				}
			}
			return 0
		})

		for i := range new_addrs {
			poi := po2idx(&new_addrs[i].TxPrevOut)
			if _, ok := CacheUnspentIdx[poi]; ok {
				fmt.Println(new_addrs[i].TxPrevOut.String(), "- already on the list")
				continue
			}

			var rec *OneCachedAddrBalance
			if new_addrs[i].BtcAddr.StealthAddr!=nil {
				var h160 [20]byte
				copy(h160[:], new_addrs[i].BtcAddr.StealthAddr.Hash160())
				rec = CachedAddrs[h160]
			} else {
				rec = CachedAddrs[new_addrs[i].BtcAddr.Hash160]
			}
			if rec==nil {
				println("Hash160 not in CachedAddrs for", new_addrs[i].BtcAddr.String())
				continue
			}
			rec.Value += new_addrs[i].Value
			CacheUnspent[rec.CacheIndex].AllUnspentTx = append(CacheUnspent[rec.CacheIndex].AllUnspentTx, new_addrs[i])
			CacheUnspentIdx[po2idx(&new_addrs[i].TxPrevOut)] = &OneCachedUnspentIdx{Index:rec.CacheIndex, Record:new_addrs[i]}
		}
		MyBalance = append(MyBalance, new_addrs...)
	}

	sort_and_sum()
}


// Calculate total balance and sort MyBalnace by block height
func sort_and_sum() {
	LastBalance = 0
	if len(MyBalance) > 0 {
		sort.Sort(MyBalance)
		for i := range MyBalance {
			LastBalance += MyBalance[i].Value
		}
	}
	BalanceInvalid = false
}


func UpdateBalanceFolder() string {
	os.RemoveAll("balance")
	os.MkdirAll("balance/", 0770)
	if BalanceInvalid {
		UpdateBalance()
	}
	utxt, _ := os.Create("balance/unspent.txt")
	return DumpBalance(MyBalance, utxt, true, false)
}

func LoadWallet(fn string) {
	MyWallet = NewWallet(fn)
	if MyWallet != nil {
		UpdateBalance()
	}
}

func FetchAllBalances() {
	dir := common.GocoinHomeDir + "wallet" + string(os.PathSeparator)
	fis, er := ioutil.ReadDir(dir)
	if er == nil {
		for i := range fis {
			if !fis[i].IsDir() && fis[i].Size()>1 {
				fpath := dir + fis[i].Name()
				//println("pre-cache wallet", fpath)
				if MyWallet==nil {
					MyWallet = NewWallet(fpath)
				} else {
					tmp := NewWallet(fpath)
					for an := range tmp.Addrs {
						var fnd bool
						for ao := range MyWallet.Addrs {
							if MyWallet.Addrs[ao].Hash160==tmp.Addrs[an].Hash160 {
								fnd = true
								break
							}
						}
						if !fnd {
							MyWallet.Addrs = append(MyWallet.Addrs, tmp.Addrs[an])
						}
					}
				}
			}
		}
	}
	if MyWallet!=nil && len(MyWallet.Addrs)>0 {
		println("Fetching balance of", len(MyWallet.Addrs), "addresses")
		UpdateBalance()
		fmt.Printf("Total cached balance: %.8f BTC in %d unspent outputs\n", float64(LastBalance)/1e8, len(MyBalance))
	}
	PrecachingComplete = true
}
